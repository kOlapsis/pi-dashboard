package kiosk

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/clock"
)

type fakeRunner struct {
	mu      sync.Mutex
	runs    []string
	starts  []string
	stopped int
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, name+" "+strings.Join(args, " "))
	return nil
}

func (f *fakeRunner) Start(_ context.Context, name string, args ...string) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, name+" "+strings.Join(args, " "))
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stopped++
	}, nil
}

func (f *fakeRunner) snapshot() (runs, starts []string, stopped int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.runs...), append([]string(nil), f.starts...), f.stopped
}

func newMachine(clk clock.Clock) (*Machine, *fakeRunner) {
	r := &fakeRunner{}
	m := NewMachine(Options{Output: "HDMI-A-1", Clock: clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Runner: r})
	return m, r
}

func state(idleS int, night bool, from, to string, events ...Event) State {
	var st State
	st.TZ = "UTC"
	st.UI.Idle.TimeoutS = idleS
	st.UI.Night.Enabled = night
	st.UI.Night.ScreenOff = true
	st.UI.Night.From, st.UI.Night.To = from, to
	st.Events = events
	return st
}

const swayidle600 = "swayidle -w timeout 600 wlopm --off HDMI-A-1 resume wlopm --on HDMI-A-1"

func TestIdleTimerFollowsSettings(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	m, r := newMachine(clk)
	ctx := context.Background()

	m.Apply(ctx, state(600, false, "", ""))
	runs, starts, stopped := r.snapshot()
	assert.Empty(t, runs)
	assert.Equal(t, []string{swayidle600}, starts)
	assert.Equal(t, 0, stopped)

	m.Apply(ctx, state(600, false, "", ""))
	_, starts, _ = r.snapshot()
	assert.Len(t, starts, 1, "unchanged settings keep the timer")

	m.Apply(ctx, state(0, false, "", ""))
	_, starts, stopped = r.snapshot()
	assert.Len(t, starts, 1)
	assert.Equal(t, 1, stopped, "timeout 0 stops swayidle")

	m.Apply(ctx, state(300, false, "", ""))
	_, starts, _ = r.snapshot()
	assert.Equal(t, "swayidle -w timeout 300 wlopm --off HDMI-A-1 resume wlopm --on HDMI-A-1", starts[len(starts)-1])
	m.Close()
	_, _, stopped = r.snapshot()
	assert.Equal(t, 2, stopped)
}

func TestEventsWakeThePanel(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	m, r := newMachine(clk)
	ctx := context.Background()
	ev := func(seq uint64, age time.Duration) Event {
		return Event{Seq: seq, At: clk.Now().Add(-age), Label: fmt.Sprint("e", seq)}
	}

	m.Apply(ctx, state(600, false, "", "", ev(5, time.Second)))
	runs, starts, _ := r.snapshot()
	assert.Empty(t, runs, "events already present at startup do not wake")
	assert.Len(t, starts, 1)

	m.Apply(ctx, state(600, false, "", "", ev(5, time.Second), ev(6, time.Second)))
	runs, starts, stopped := r.snapshot()
	assert.Equal(t, []string{"wlopm --on HDMI-A-1"}, runs)
	assert.Len(t, starts, 2, "the idle timer is re-armed")
	assert.Equal(t, 1, stopped)

	m.Apply(ctx, state(600, false, "", "", ev(6, time.Second)))
	runs, _, _ = r.snapshot()
	assert.Len(t, runs, 1, "same event again is ignored")

	m.Apply(ctx, state(600, false, "", "", ev(7, 20*time.Minute)))
	runs, _, _ = r.snapshot()
	assert.Len(t, runs, 1, "an old event does not wake")

	m.Apply(ctx, state(600, false, "", "", ev(2, time.Second)))
	runs, _, _ = r.snapshot()
	assert.Len(t, runs, 2, "a new sequence after a server restart still wakes")
}

func TestNightTurnsThePanelOff(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 7, 23, 30, 0, 0, time.UTC))
	m, r := newMachine(clk)
	ctx := context.Background()

	m.Apply(ctx, state(600, true, "00:00", "08:30", Event{Seq: 1, At: clk.Now()}))
	runs, starts, _ := r.snapshot()
	assert.Empty(t, runs)
	assert.Len(t, starts, 1)

	clk.Advance(31 * time.Minute)
	m.Tick(ctx, clk.Now())
	runs, _, _ = r.snapshot()
	assert.Equal(t, []string{"wlopm --off HDMI-A-1"}, runs)

	m.Apply(ctx, state(600, true, "00:00", "08:30", Event{Seq: 2, At: clk.Now()}))
	runs, _, _ = r.snapshot()
	assert.Len(t, runs, 1, "no wake on events at night")

	clk.Advance(8 * time.Hour)
	m.Tick(ctx, clk.Now())
	runs, _, _ = r.snapshot()
	assert.Equal(t, "wlopm --off HDMI-A-1", runs[len(runs)-1], "08:01 is still night")

	clk.Advance(30 * time.Minute)
	m.Tick(ctx, clk.Now())
	runs, starts, stopped := r.snapshot()
	assert.Equal(t, "wlopm --on HDMI-A-1", runs[len(runs)-1])
	assert.Len(t, starts, 2, "the idle timer is re-armed in the morning")
	assert.Equal(t, 1, stopped)

	m.Apply(ctx, state(600, true, "00:00", "08:30", Event{Seq: 3, At: clk.Now()}))
	runs, _, _ = r.snapshot()
	assert.Equal(t, "wlopm --on HDMI-A-1", runs[len(runs)-1])
	assert.Len(t, runs, 3, "events wake again during the day")
}

func TestNightDisabledByScreenOff(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC))
	m, r := newMachine(clk)
	st := state(600, true, "00:00", "08:30")
	st.UI.Night.ScreenOff = false
	m.Apply(context.Background(), st)
	runs, _, _ := r.snapshot()
	assert.Empty(t, runs)
}

func TestReadEvents(t *testing.T) {
	stream := "retry: 3000\n\n" +
		"event: state\ndata: {\"tz\":\"Europe/Paris\",\"ui\":{\"idle\":{\"timeout_s\":600}},\"events\":[{\"seq\":3,\"label\":\"a\"}]}\n\n" +
		": ping\n\n" +
		"event: other\ndata: {}\n\n" +
		"event: state\ndata: {\"events\":[]}\n\n"
	var got []State
	err := ReadEvents(strings.NewReader(stream), func(st State) { got = append(got, st) })
	assert.EqualError(t, err, "stream closed")
	require.Len(t, got, 2)
	assert.Equal(t, 600, got[0].UI.Idle.TimeoutS)
	assert.Equal(t, uint64(3), got[0].Events[0].Seq)
	assert.Empty(t, got[1].Events)
}

func TestRunFollowsTheStream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: state\ndata: {\"tz\":\"UTC\",\"ui\":{\"idle\":{\"timeout_s\":600}}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()
	r := &fakeRunner{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{URL: ts.URL + "/", Output: "HDMI-A-1", Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Runner: r})
	}()
	require.Eventually(t, func() bool {
		_, starts, _ := r.snapshot()
		return len(starts) == 1 && starts[0] == swayidle600
	}, 5*time.Second, 10*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	_, _, stopped := r.snapshot()
	assert.Equal(t, 1, stopped, "swayidle is stopped on exit")
}
