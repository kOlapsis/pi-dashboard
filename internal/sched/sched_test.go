package sched

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/clock"
)

type fakeCollector struct {
	name     string
	interval time.Duration
	fail     atomic.Bool
	calls    atomic.Int32
}

func (f *fakeCollector) Name() string            { return f.name }
func (f *fakeCollector) Interval() time.Duration { return f.interval }
func (f *fakeCollector) Collect(context.Context) (any, error) {
	n := f.calls.Add(1)
	if f.fail.Load() {
		return nil, errors.New("boom")
	}
	return map[string]int{"n": int(n)}, nil
}

func newTestScheduler(clk clock.Clock) *Scheduler {
	return New(clk, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{Rand: rand.New(rand.NewPCG(1, 2))})
}

func advanceUntil(t *testing.T, clk *clock.Fake, step time.Duration, cond func() bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		if cond() {
			return true
		}
		clk.Advance(step)
		return cond()
	}, 5*time.Second, 5*time.Millisecond)
}

func TestLifecycleOkStaleError(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC))
	s := newTestScheduler(clk)
	c := &fakeCollector{name: "x", interval: time.Minute}
	s.Add(c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()

	state := func() State { return s.Snapshot().Collectors["x"].Status.State }
	assert.Equal(t, StateInit, state())

	advanceUntil(t, clk, time.Second, func() bool { return state() == StateOK })
	e := s.Snapshot().Collectors["x"]
	assert.JSONEq(t, `{"n":1}`, string(e.Data))
	assert.Equal(t, 60, e.Status.IntervalS)

	c.fail.Store(true)
	advanceUntil(t, clk, 10*time.Second, func() bool { return state() == StateStale })
	e = s.Snapshot().Collectors["x"]
	assert.JSONEq(t, `{"n":1}`, string(e.Data), "last good data is kept while stale")
	assert.Equal(t, "boom", e.Status.Error)
	assert.Equal(t, 1, e.Status.Failures)

	advanceUntil(t, clk, time.Minute, func() bool { return state() == StateError })
	e = s.Snapshot().Collectors["x"]
	assert.JSONEq(t, `{"n":1}`, string(e.Data))
	assert.Greater(t, e.Status.Failures, 1)

	c.fail.Store(false)
	advanceUntil(t, clk, time.Minute, func() bool { return state() == StateOK })
	assert.Equal(t, 0, s.Snapshot().Collectors["x"].Status.Failures)
}

func TestRestoreSeedsStaleData(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC))
	s := newTestScheduler(clk)
	s.Add(&fakeCollector{name: "x", interval: time.Minute}, &fakeCollector{name: "y", interval: time.Minute})
	lastOK := clk.Now().Add(-time.Hour)
	s.Restore(Snapshot{Collectors: map[string]Entry{
		"x":    {Status: Status{State: StateOK, LastOK: lastOK}, Data: json.RawMessage(`{"n":42}`)},
		"gone": {Status: Status{State: StateOK}, Data: json.RawMessage(`{}`)},
	}})
	snap := s.Snapshot()
	assert.Equal(t, StateStale, snap.Collectors["x"].Status.State)
	assert.Equal(t, lastOK, snap.Collectors["x"].Status.LastOK)
	assert.JSONEq(t, `{"n":42}`, string(snap.Collectors["x"].Data))
	assert.Equal(t, StateInit, snap.Collectors["y"].Status.State)
	assert.NotContains(t, snap.Collectors, "gone")
}

func TestSubscribeGetsLatestSnapshot(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC))
	s := newTestScheduler(clk)
	s.Add(&fakeCollector{name: "x", interval: time.Minute})
	ch, cancel := s.Subscribe()
	defer cancel()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() { _ = s.Run(ctx) }()

	var got Snapshot
	require.Eventually(t, func() bool {
		clk.Advance(time.Second)
		select {
		case got = <-ch:
			return true
		default:
			return false
		}
	}, 5*time.Second, 5*time.Millisecond)
	assert.Equal(t, StateOK, got.Collectors["x"].Status.State)
	assert.NotZero(t, got.Seq)
}

func TestCollectTimeoutAndPanic(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC))
	s := New(clk, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{DefaultTimeout: 20 * time.Millisecond})
	_, err := s.collect(context.Background(), collectorFunc(func(ctx context.Context) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}))
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	_, err = s.collect(context.Background(), collectorFunc(func(context.Context) (any, error) { panic("oops") }))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "panic: oops")
}

type collectorFunc func(ctx context.Context) (any, error)

func (f collectorFunc) Name() string                             { return "f" }
func (f collectorFunc) Interval() time.Duration                  { return time.Minute }
func (f collectorFunc) Collect(ctx context.Context) (any, error) { return f(ctx) }

func TestBackoff(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, time.Minute},
		{1, 2 * time.Minute},
		{2, 4 * time.Minute},
		{3, 8 * time.Minute},
		{4, 15 * time.Minute},
		{10, 15 * time.Minute},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, backoff(time.Minute, c.failures, 15*time.Minute), "failures=%d", c.failures)
	}
}

func TestJitterBounds(t *testing.T) {
	s := newTestScheduler(clock.NewFake(time.Time{}))
	for range 200 {
		d := s.jitter(time.Minute)
		assert.GreaterOrEqual(t, d, 54*time.Second)
		assert.LessOrEqual(t, d, 66*time.Second)
	}
	assert.Zero(t, s.jitter(0))
}

type notifyingCollector struct {
	fakeCollector
	seen []any
}

func (n *notifyingCollector) Notify(prev, cur any) []string {
	n.seen = append(n.seen, prev)
	p, c := prev.(map[string]int), cur.(map[string]int)
	if c["n"] > p["n"] {
		return []string{"n went up"}
	}
	return nil
}

func TestNotifierEvents(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC))
	s := newTestScheduler(clk)
	c := &notifyingCollector{fakeCollector: fakeCollector{name: "x", interval: time.Minute}}
	s.Add(c)
	s.Restore(Snapshot{
		Collectors: map[string]Entry{"x": {Data: json.RawMessage(`{"n":0}`)}},
		Events:     []Event{{Seq: 41, At: clk.Now().Add(-time.Hour), Collector: "x", Label: "old"}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()

	events := func() []Event { return s.Snapshot().Events }
	advanceUntil(t, clk, time.Second, func() bool { return len(events()) == 2 })
	ev := events()[1]
	assert.Equal(t, uint64(42), ev.Seq, "sequence continues after the restored events")
	assert.Equal(t, "x", ev.Collector)
	assert.Equal(t, "n went up", ev.Label)
	assert.Equal(t, clk.Now(), ev.At)
	assert.Equal(t, map[string]int{"n": 0}, c.seen[0], "the restored snapshot is decoded into the collector's type")

	advanceUntil(t, clk, time.Minute, func() bool { return len(events()) == 3 })
	assert.Equal(t, map[string]int{"n": 1}, c.seen[1], "then the previous typed value is used")
	assert.Equal(t, uint64(43), events()[2].Seq)
}

func TestEventsAreCapped(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC))
	s := newTestScheduler(clk)
	c := &notifyingCollector{fakeCollector: fakeCollector{name: "x", interval: time.Minute}}
	s.Add(c)
	s.Restore(Snapshot{Collectors: map[string]Entry{"x": {Data: json.RawMessage(`{"n":0}`)}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	advanceUntil(t, clk, time.Minute, func() bool { return int(c.calls.Load()) >= maxEvents+5 })
	evs := s.Snapshot().Events
	require.Len(t, evs, maxEvents)
	assert.Equal(t, evs[len(evs)-1].Seq-uint64(maxEvents-1), evs[0].Seq)
}

type panickyCollector struct{ fakeCollector }

func (*panickyCollector) Notify(any, any) []string { panic("nope") }

func TestNotifierPanicIsContained(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC))
	s := newTestScheduler(clk)
	c := &panickyCollector{fakeCollector{name: "x", interval: time.Minute}}
	s.Add(c)
	s.Restore(Snapshot{Collectors: map[string]Entry{"x": {Data: json.RawMessage(`{"n":0}`)}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = s.Run(ctx) }()
	advanceUntil(t, clk, time.Second, func() bool { return s.Snapshot().Collectors["x"].Status.State == StateOK })
	assert.Empty(t, s.Snapshot().Events)
}
