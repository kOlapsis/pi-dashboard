package sched

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"reflect"
	"sync"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/collector"
)

type State string

const (
	StateInit  State = "init"
	StateOK    State = "ok"
	StateStale State = "stale"
	StateError State = "error"
)

type Status struct {
	State     State     `json:"state"`
	LastOK    time.Time `json:"last_ok"`
	LastTry   time.Time `json:"last_try"`
	Error     string    `json:"error,omitempty"`
	Failures  int       `json:"failures"`
	IntervalS int       `json:"interval_s"`
}

type Entry struct {
	Status Status          `json:"status"`
	Data   json.RawMessage `json:"data,omitempty"`
}

type Event struct {
	Seq       uint64    `json:"seq"`
	At        time.Time `json:"at"`
	Collector string    `json:"collector"`
	Label     string    `json:"label"`
}

type Snapshot struct {
	Seq        uint64           `json:"seq"`
	Now        time.Time        `json:"now"`
	Collectors map[string]Entry `json:"collectors"`
	Events     []Event          `json:"events,omitempty"`
}

type Options struct {
	MaxBackoff     time.Duration
	DefaultTimeout time.Duration
	Coalesce       time.Duration
	MaxFirstDelay  time.Duration
	Rand           *rand.Rand
}

const (
	staleGrace = 15 * time.Minute
	maxEvents  = 20
)

type Scheduler struct {
	clock clock.Clock
	log   *slog.Logger
	opts  Options

	mu      sync.RWMutex
	cols    []collector.Collector
	entries map[string]*Entry
	last    map[string]any
	events  []Event
	evSeq   uint64
	seq     uint64
	subs    map[chan Snapshot]struct{}
	dirty   chan struct{}
}

func New(clk clock.Clock, log *slog.Logger, opts Options) *Scheduler {
	if opts.MaxBackoff == 0 {
		opts.MaxBackoff = 15 * time.Minute
	}
	if opts.DefaultTimeout == 0 {
		opts.DefaultTimeout = 20 * time.Second
	}
	if opts.Coalesce == 0 {
		opts.Coalesce = 250 * time.Millisecond
	}
	if opts.MaxFirstDelay == 0 {
		opts.MaxFirstDelay = 10 * time.Second
	}
	if opts.Rand == nil {
		opts.Rand = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	return &Scheduler{
		clock:   clk,
		log:     log,
		opts:    opts,
		entries: map[string]*Entry{},
		last:    map[string]any{},
		subs:    map[chan Snapshot]struct{}{},
		dirty:   make(chan struct{}, 1),
	}
}

func (s *Scheduler) Add(cs ...collector.Collector) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range cs {
		s.cols = append(s.cols, c)
		s.entries[c.Name()] = &Entry{Status: Status{State: StateInit, IntervalS: int(c.Interval() / time.Second)}}
	}
}

func (s *Scheduler) Collectors() []collector.Collector {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]collector.Collector(nil), s.cols...)
}

// Restore seeds entries from a previous snapshot so the screen is never empty after a restart.
func (s *Scheduler) Restore(prev Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, e := range s.entries {
		p, ok := prev.Collectors[name]
		if !ok || len(p.Data) == 0 {
			continue
		}
		e.Data = p.Data
		e.Status.LastOK = p.Status.LastOK
		e.Status.State = StateStale
	}
	s.events = trimEvents(append([]Event(nil), prev.Events...))
	for _, ev := range s.events {
		s.evSeq = max(s.evSeq, ev.Seq)
	}
	s.seq++
}

func (s *Scheduler) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, c := range s.Collectors() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.loop(ctx, c)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.notify(ctx)
	}()
	wg.Wait()
	return ctx.Err()
}

func (s *Scheduler) loop(ctx context.Context, c collector.Collector) {
	interval := c.Interval()
	wait := s.jitter(min(interval, s.opts.MaxFirstDelay))
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.clock.After(wait):
		}
		data, err := s.collect(ctx, c)
		if ctx.Err() != nil {
			return
		}
		s.record(c, data, err)
		if err != nil {
			failures++
			wait = s.jitter(backoff(interval, failures, s.opts.MaxBackoff))
			s.log.Warn("collect failed", "collector", c.Name(), "failures", failures, "retry_in", wait.Round(time.Second), "err", err)
			continue
		}
		failures = 0
		wait = s.jitter(interval)
	}
}

func backoff(interval time.Duration, failures int, maxBackoff time.Duration) time.Duration {
	d := interval
	for i := 0; i < failures && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

func (s *Scheduler) jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	f := 0.9 + 0.2*s.opts.Rand.Float64()
	return time.Duration(float64(d) * f)
}

func (s *Scheduler) collect(ctx context.Context, c collector.Collector) (data any, err error) {
	timeout := s.opts.DefaultTimeout
	if t, ok := c.(collector.Timeouter); ok && t.Timeout() > 0 {
		timeout = t.Timeout()
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return c.Collect(cctx)
}

func (s *Scheduler) record(c collector.Collector, data any, err error) {
	now := s.clock.Now()
	s.mu.Lock()
	e := s.entries[c.Name()]
	e.Status.LastTry = now
	if err == nil {
		raw, merr := json.Marshal(data)
		if merr != nil {
			err = fmt.Errorf("encode: %w", merr)
		} else {
			s.addEvents(c, now, s.changes(c, e, data))
			s.last[c.Name()] = data
			e.Data = raw
			e.Status.LastOK = now
			e.Status.State = StateOK
			e.Status.Error = ""
			e.Status.Failures = 0
		}
	}
	if err != nil {
		e.Status.Failures++
		e.Status.Error = err.Error()
		grace := max(3*c.Interval(), staleGrace)
		if !e.Status.LastOK.IsZero() && now.Sub(e.Status.LastOK) <= grace {
			e.Status.State = StateStale
		} else {
			e.Status.State = StateError
		}
	}
	s.seq++
	s.mu.Unlock()
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

func (s *Scheduler) changes(c collector.Collector, e *Entry, data any) (labels []string) {
	n, ok := c.(collector.Notifier)
	if !ok || data == nil {
		return nil
	}
	prev := s.last[c.Name()]
	if prev == nil && len(e.Data) > 0 {
		prev = decodeAs(e.Data, data)
	}
	if prev == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			s.log.Warn("notify panicked", "collector", c.Name(), "panic", r)
			labels = nil
		}
	}()
	return n.Notify(prev, data)
}

func decodeAs(raw json.RawMessage, like any) any {
	v := reflect.New(reflect.TypeOf(like))
	if err := json.Unmarshal(raw, v.Interface()); err != nil {
		return nil
	}
	return v.Elem().Interface()
}

func (s *Scheduler) addEvents(c collector.Collector, now time.Time, labels []string) {
	for _, label := range labels {
		s.evSeq++
		s.events = append(s.events, Event{Seq: s.evSeq, At: now, Collector: c.Name(), Label: label})
		s.log.Info("event", "collector", c.Name(), "label", label)
	}
	s.events = trimEvents(s.events)
}

func trimEvents(evs []Event) []Event {
	if len(evs) > maxEvents {
		return evs[len(evs)-maxEvents:]
	}
	return evs
}

func (s *Scheduler) notify(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.dirty:
		}
		select {
		case <-ctx.Done():
			return
		case <-s.clock.After(s.opts.Coalesce):
		}
		snap := s.Snapshot()
		s.mu.RLock()
		for ch := range s.subs {
			select {
			case <-ch:
			default:
			}
			ch <- snap
		}
		s.mu.RUnlock()
	}
}

func (s *Scheduler) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := Snapshot{Seq: s.seq, Now: s.clock.Now(), Collectors: make(map[string]Entry, len(s.entries)), Events: append([]Event(nil), s.events...)}
	for name, e := range s.entries {
		out.Collectors[name] = *e
	}
	return out
}

func (s *Scheduler) Subscribe() (<-chan Snapshot, func()) {
	ch := make(chan Snapshot, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}
}
