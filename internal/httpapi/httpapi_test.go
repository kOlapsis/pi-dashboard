package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/sched"
)

type stub struct{}

func (stub) Name() string                         { return "weather" }
func (stub) Interval() time.Duration              { return time.Minute }
func (stub) Collect(context.Context) (any, error) { return map[string]int{"temp": 14}, nil }

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC))
	s := sched.New(clk, slog.New(slog.NewTextHandler(io.Discard, nil)), sched.Options{})
	s.Add(stub{})
	s.Restore(sched.Snapshot{
		Collectors: map[string]sched.Entry{"weather": {Status: sched.Status{LastOK: clk.Now().Add(-time.Hour)}, Data: json.RawMessage(`{"temp":12}`)}},
		Events:     []sched.Event{{Seq: 7, At: clk.Now().Add(-time.Minute), Collector: "mail", Label: "Mail de Alice"}},
	})
	srv := &Server{
		Sched:   s,
		Static:  fstest.MapFS{"index.html": {Data: []byte("<html>ok</html>")}, "style.css": {Data: []byte("body{}")}, "fonts/a.woff2": {Data: []byte("x")}},
		Clock:   clk,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version: "1.2.3",
		TZ:      "Europe/Paris",
		UI:      UI{Scale: "auto", Idle: Idle{TimeoutS: 600}, Night: Night{Enabled: true, From: "23:00", To: "07:00", Brightness: 0.35, ScreenOff: true}},
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestState(t *testing.T) {
	ts := newServer(t)
	resp, err := http.Get(ts.URL + "/api/state")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "default-src 'self'")
	var st State
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&st))
	assert.Equal(t, "1.2.3", st.Version)
	assert.Equal(t, "Europe/Paris", st.TZ)
	assert.Equal(t, "23:00", st.UI.Night.From)
	assert.True(t, st.UI.Night.ScreenOff)
	assert.Equal(t, 600, st.UI.Idle.TimeoutS)
	require.Len(t, st.Events, 1)
	assert.Equal(t, uint64(7), st.Events[0].Seq)
	assert.Equal(t, "Mail de Alice", st.Events[0].Label)
	e := st.Collectors["weather"]
	assert.Equal(t, sched.StateStale, e.Status.State)
	assert.JSONEq(t, `{"temp":12}`, string(e.Data))
}

func TestStaticCaching(t *testing.T) {
	ts := newServer(t)
	for path, cache := range map[string]string{"/": "no-store", "/style.css": "no-cache", "/fonts/a.woff2": "public, max-age=31536000, immutable"} {
		resp, err := http.Get(ts.URL + path)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.Equal(t, http.StatusOK, resp.StatusCode, path)
		assert.Equal(t, cache, resp.Header.Get("Cache-Control"), path)
	}
	resp, err := http.Get(ts.URL + "/healthz")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestEventsSendsInitialState(t *testing.T) {
	ts := newServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	r := bufio.NewReader(resp.Body)
	var lines []string
	for len(lines) < 3 {
		line, err := r.ReadString('\n')
		require.NoError(t, err)
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	assert.Equal(t, "retry: 3000", lines[0])
	assert.Equal(t, "event: state", lines[1])
	require.True(t, strings.HasPrefix(lines[2], "data: "))
	var st State
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(lines[2], "data: ")), &st))
	assert.Contains(t, st.Collectors, "weather")
}
