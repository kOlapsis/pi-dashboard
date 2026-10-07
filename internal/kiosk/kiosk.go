// Package kiosk runs inside the labwc session: it turns the panel off when idle or at night, and back on
// when touched or when the dashboard reports a new event.
package kiosk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/night"
)

const (
	eventFreshness = 10 * time.Minute
	maxReconnect   = 30 * time.Second
	tick           = time.Minute
)

type Runner interface {
	Run(ctx context.Context, name string, args ...string) error
	Start(ctx context.Context, name string, args ...string) (stop func(), err error)
}

type Options struct {
	URL    string
	Output string
	Clock  clock.Clock
	Log    *slog.Logger
	HTTP   *http.Client
	Runner Runner
}

type State struct {
	Now    time.Time `json:"now"`
	TZ     string    `json:"tz"`
	UI     UI        `json:"ui"`
	Events []Event   `json:"events"`
}

type UI struct {
	Idle struct {
		TimeoutS int `json:"timeout_s"`
	} `json:"idle"`
	Night struct {
		Enabled   bool   `json:"enabled"`
		From      string `json:"from"`
		To        string `json:"to"`
		ScreenOff bool   `json:"screen_off"`
	} `json:"night"`
}

type Event struct {
	Seq   uint64    `json:"seq"`
	At    time.Time `json:"at"`
	Label string    `json:"label"`
}

type Machine struct {
	opts     Options
	idle     time.Duration
	window   night.Window
	nightOff bool
	loc      *time.Location
	night    bool
	lastSeq  uint64
	haveSeq  bool
	stopIdle func()
}

func NewMachine(opts Options) *Machine {
	return &Machine{opts: opts, loc: time.Local}
}

// Apply absorbs a dashboard state: settings, night window and new events.
func (m *Machine) Apply(ctx context.Context, st State) {
	now := m.opts.Clock.Now()
	if st.TZ != "" {
		if loc, err := time.LoadLocation(st.TZ); err == nil {
			m.loc = loc
		}
	}
	idle := time.Duration(st.UI.Idle.TimeoutS) * time.Second
	window := night.Window{From: st.UI.Night.From, To: st.UI.Night.To}
	nightOff := st.UI.Night.Enabled && st.UI.Night.ScreenOff && window.Validate() == nil
	if idle != m.idle || window != m.window || nightOff != m.nightOff {
		m.idle, m.window, m.nightOff = idle, window, nightOff
		m.restartIdle(ctx)
	}
	m.Tick(ctx, now)
	if len(st.Events) == 0 {
		return
	}
	last := st.Events[len(st.Events)-1]
	if !m.haveSeq {
		m.lastSeq, m.haveSeq = last.Seq, true
		return
	}
	if last.Seq == m.lastSeq {
		return
	}
	m.lastSeq = last.Seq
	if m.night {
		m.opts.Log.Debug("event ignored at night", "label", last.Label)
		return
	}
	if now.Sub(last.At) > eventFreshness {
		m.opts.Log.Debug("event too old to wake", "label", last.Label, "at", last.At)
		return
	}
	m.opts.Log.Info("wake", "label", last.Label)
	m.panel(ctx, true)
	m.restartIdle(ctx)
}

// Tick applies the night window at its edges.
func (m *Machine) Tick(ctx context.Context, now time.Time) {
	active := m.nightOff && m.window.Active(now.In(m.loc))
	if active == m.night {
		return
	}
	m.night = active
	m.opts.Log.Info("night", "active", active)
	m.panel(ctx, !active)
	if !active {
		m.restartIdle(ctx)
	}
}

func (m *Machine) Close() {
	if m.stopIdle != nil {
		m.stopIdle()
		m.stopIdle = nil
	}
}

func (m *Machine) panel(ctx context.Context, on bool) {
	mode := "--off"
	if on {
		mode = "--on"
	}
	if err := m.opts.Runner.Run(ctx, "wlopm", mode, m.opts.Output); err != nil {
		m.opts.Log.Warn("wlopm failed", "mode", mode, "err", err)
	}
}

func (m *Machine) restartIdle(ctx context.Context) {
	m.Close()
	if m.idle <= 0 {
		return
	}
	secs := strconv.Itoa(int(m.idle / time.Second))
	stop, err := m.opts.Runner.Start(ctx, "swayidle", "-w",
		"timeout", secs, "wlopm --off "+m.opts.Output,
		"resume", "wlopm --on "+m.opts.Output)
	if err != nil {
		m.opts.Log.Warn("swayidle failed", "err", err)
		return
	}
	m.stopIdle = stop
}

// Run follows the dashboard's event stream until ctx is done.
func Run(ctx context.Context, opts Options) error {
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 10 * time.Second}}
	}
	if opts.Runner == nil {
		opts.Runner = ExecRunner{Log: opts.Log}
	}
	m := NewMachine(opts)
	defer m.Close()
	states := make(chan State, 1)
	go stream(ctx, opts, states)
	for {
		select {
		case <-ctx.Done():
			return nil
		case st := <-states:
			m.Apply(ctx, st)
		case <-opts.Clock.After(tick):
			m.Tick(ctx, opts.Clock.Now())
		}
	}
}

func stream(ctx context.Context, opts Options, out chan State) {
	delay := time.Second
	for {
		err := follow(ctx, opts, out)
		if ctx.Err() != nil {
			return
		}
		opts.Log.Warn("event stream lost", "err", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return
		case <-opts.Clock.After(delay):
		}
		delay = min(delay*2, maxReconnect)
	}
}

func follow(ctx context.Context, opts Options, out chan State) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(opts.URL, "/")+"/api/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := opts.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return ReadEvents(resp.Body, func(st State) {
		select {
		case <-out:
		default:
		}
		out <- st
	})
}

// ReadEvents parses a server-sent event stream and hands every "state" event to fn.
func ReadEvents(r io.Reader, fn func(State)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	var event, data strings.Builder
	flush := func() {
		if event.String() == "state" && data.Len() > 0 {
			var st State
			if err := json.Unmarshal([]byte(data.String()), &st); err == nil {
				fn(st)
			}
		}
		event.Reset()
		data.Reset()
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "event:"):
			event.WriteString(strings.TrimSpace(line[6:]))
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(line[5:], " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("stream closed")
}
