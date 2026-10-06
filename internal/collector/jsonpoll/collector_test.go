package jsonpoll

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/store"
)

var (
	_ collector.Collector  = (*Collector)(nil)
	_ collector.Summarizer = (*Collector)(nil)

	t0 = time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC)
)

type env struct {
	c    *Collector
	clk  *clock.Fake
	hist *store.Mem
	seen chan *http.Request
}

func shmStats(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "shm_stats.json"))
	require.NoError(t, err)
	return string(b)
}

func newEnv(t *testing.T, cfg config.JSONPoll, handler http.HandlerFunc) *env {
	t.Helper()
	e := &env{clk: clock.NewFake(t0), hist: store.NewMem(), seen: make(chan *http.Request, 16)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.seen <- r.Clone(context.Background())
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	if cfg.URL == "" {
		cfg.URL = srv.URL + "/api/v1/admin/stats"
	}
	e.c = New(cfg, collector.Deps{
		HTTP:  srv.Client(),
		Clock: e.clk,
		Hist:  e.hist,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Loc:   time.UTC,
	})
	return e
}

func body(status int, s string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, s)
	}
}

func poll(name string, items ...config.JSONPollItem) config.JSONPoll {
	return config.JSONPoll{Name: name, Interval: 15 * time.Minute, Items: items}
}

func item(key, path, format string) config.JSONPollItem {
	return config.JSONPollItem{Key: key, Label: strings.ToUpper(key[:1]) + key[1:], Path: path, Format: format}
}

func (e *env) collect(t *testing.T) Data {
	t.Helper()
	got, err := e.c.Collect(context.Background())
	require.NoError(t, err)
	return got.(Data)
}

func ptr[T any](v T) *T { return &v }

func shmConfig() config.JSONPoll {
	cfg := poll("shm",
		config.JSONPollItem{Key: "instances", Label: "Instances", Path: "per_app_counts.Maintenant", History: true},
		config.JSONPollItem{Key: "active", Label: "Actives", Path: "active_instances"},
	)
	cfg.BasicAuth = &config.BasicAuth{User: "admin", Password: "s3cret"}
	cfg.Headers = map[string]string{"X-Trace": "dash"}
	return cfg
}

func TestCollect(t *testing.T) {
	e := newEnv(t, shmConfig(), body(http.StatusOK, shmStats(t)))

	got := e.collect(t)

	assert.Equal(t, Data{Name: "shm", Items: []Item{
		{Key: "instances", Label: "Instances", Value: 214, Text: "214"},
		{Key: "active", Label: "Actives", Value: 131, Text: "131"},
	}}, got)
	assert.Equal(t, "instances=214 active=131", e.c.Summary(got))
}

func TestRequest(t *testing.T) {
	e := newEnv(t, shmConfig(), body(http.StatusOK, shmStats(t)))

	e.collect(t)

	r := <-e.seen
	user, pass, ok := r.BasicAuth()
	assert.True(t, ok)
	assert.Equal(t, "admin", user)
	assert.Equal(t, "s3cret", pass)
	assert.Equal(t, "dash", r.Header.Get("X-Trace"))
	assert.Equal(t, "application/json", r.Header.Get("Accept"))
	assert.Equal(t, http.MethodGet, r.Method)
	assert.Equal(t, "/api/v1/admin/stats", r.URL.Path)
	assert.Contains(t, r.Header.Get("User-Agent"), "pi-dashboard")
}

func TestAuthorizationSources(t *testing.T) {
	tests := []struct {
		name      string
		headers   map[string]string
		basicAuth *config.BasicAuth
		want      string
	}{
		{"none", nil, nil, ""},
		{"header only", map[string]string{"Authorization": "Bearer abc"}, nil, "Bearer abc"},
		{"basic auth only", nil, &config.BasicAuth{User: "u", Password: "p"}, "Basic dTpw"},
		{"basic auth wins over the header", map[string]string{"Authorization": "Bearer abc"}, &config.BasicAuth{User: "u", Password: "p"}, "Basic dTpw"},
		{"password with a colon", nil, &config.BasicAuth{User: "u", Password: "a:b"}, "Basic dTphOmI="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := poll("shm", item("active", "active_instances", ""))
			cfg.Headers, cfg.BasicAuth = tt.headers, tt.basicAuth
			e := newEnv(t, cfg, body(http.StatusOK, shmStats(t)))

			e.collect(t)

			assert.Equal(t, tt.want, (<-e.seen).Header.Get("Authorization"))
		})
	}
}

func TestConfiguredHeadersAreNotMutated(t *testing.T) {
	cfg := shmConfig()
	e := newEnv(t, cfg, body(http.StatusOK, shmStats(t)))

	e.collect(t)

	assert.Equal(t, map[string]string{"X-Trace": "dash"}, e.c.cfg.Headers)
}

func TestHistoryAndDelta7(t *testing.T) {
	var current atomic.Value
	current.Store(shmStats(t))
	e := newEnv(t, shmConfig(), func(w http.ResponseWriter, r *http.Request) {
		body(http.StatusOK, current.Load().(string))(w, r)
	})
	first := e.collect(t)
	assert.Nil(t, first.Items[0].Delta7)
	ctx := context.Background()

	v, ok, err := e.hist.At(ctx, "shm", "instances", t0)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.EqualValues(t, 214, v)
	_, ok, err = e.hist.At(ctx, "shm", "active", t0)
	require.NoError(t, err)
	assert.False(t, ok, "items without history are not recorded")

	e.clk.Advance(24 * time.Hour)
	assert.Nil(t, e.collect(t).Items[0].Delta7, "one day of history is not a week")

	e.clk.Advance(6 * 24 * time.Hour)
	current.Store(strings.Replace(shmStats(t), `"Maintenant": 214`, `"Maintenant": 220`, 1))
	second := e.collect(t)

	assert.Equal(t, 220.0, second.Items[0].Value)
	assert.Equal(t, ptr(6.0), second.Items[0].Delta7)
	assert.Nil(t, second.Items[1].Delta7)
}

func TestRendering(t *testing.T) {
	tests := []struct {
		name      string
		json      string
		format    string
		wantValue float64
		wantText  string
	}{
		{"integer", `{"v": 214}`, "", 214, "214"},
		{"decimal", `{"v": 12.5}`, "", 12.5, "12.5"},
		{"large number has no exponent", `{"v": 1234567}`, "", 1234567, "1234567"},
		{"negative", `{"v": -3}`, "", -3, "-3"},
		{"string", `{"v": "ok"}`, "", 0, "ok"},
		{"numeric string", `{"v": "12.5"}`, "", 12.5, "12.5"},
		{"true", `{"v": true}`, "", 1, "true"},
		{"false", `{"v": false}`, "", 0, "false"},
		{"int rounds half up", `{"v": 12.5}`, "int", 12.5, "13"},
		{"int rounds down", `{"v": 12.4}`, "int", 12.4, "12"},
		{"int rounds negatives away from zero", `{"v": -12.5}`, "int", -12.5, "-13"},
		{"int never prints negative zero", `{"v": -0.4}`, "int", -0.4, "0"},
		{"int of an exact integer", `{"v": 131}`, "int", 131, "131"},
		{"int of a string", `{"v": "7.6"}`, "int", 7.6, "8"},
		{"int of a huge number", `{"v": 1e20}`, "int", 1e20, "100000000000000000000"},
		{"float has one decimal", `{"v": 12.54}`, "float", 12.54, "12.5"},
		{"float pads", `{"v": 3}`, "float", 3, "3.0"},
		{"percent", `{"v": 12.5}`, "percent", 12.5, "12.5%"},
		{"percent of an integer", `{"v": 50}`, "percent", 50, "50.0%"},
		{"formatting a word keeps the word", `{"v": "n/a"}`, "int", 0, "n/a"},
		{"formatting a boolean keeps the word", `{"v": true}`, "float", 1, "true"},
		{"not a number is not a number", `{"v": "NaN"}`, "int", 0, "NaN"},
		{"infinity is not a number", `{"v": "-Infinity"}`, "float", 0, "-Infinity"},
		{"array keeps its json", `{"v": [1, 2]}`, "", 0, "[1, 2]"},
		{"object keeps its json", `{"v": {"a": 1}}`, "int", 0, `{"a": 1}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, poll("x", item("v", "v", tt.format)), body(http.StatusOK, tt.json))

			got := e.collect(t).Items[0]

			assert.InDelta(t, tt.wantValue, got.Value, 1e-9)
			assert.Equal(t, tt.wantText, got.Text)
		})
	}
}

func TestPathSyntax(t *testing.T) {
	const doc = `{
		"a.b": 5,
		"nested": {"list": [{"up": true}, {"up": false}, {"up": true}]},
		"per_app": {"x": 1, "y": 2, "z": 3}
	}`
	tests := []struct {
		name     string
		path     string
		wantText string
	}{
		{"escaped dot", `a\.b`, "5"},
		{"array index", "nested.list.1.up", "false"},
		{"array length", "nested.list.#", "3"},
		{"count of matches", "nested.list.#(up==true)#|#", "2"},
		{"object key count", "per_app.@keys.#", "3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, poll("x", item("v", tt.path, "")), body(http.StatusOK, doc))

			assert.Equal(t, tt.wantText, e.collect(t).Items[0].Text)
		})
	}
}

func TestLabelFallsBackToKey(t *testing.T) {
	cfg := poll("x", config.JSONPollItem{Key: "active", Path: "active_instances"})
	e := newEnv(t, cfg, body(http.StatusOK, shmStats(t)))

	assert.Equal(t, "active", e.collect(t).Items[0].Label)
}

func TestMissingPathsFailLoudly(t *testing.T) {
	cfg := poll("shm",
		config.JSONPollItem{Key: "instances", Path: "per_app_counts.Maintenant", History: true},
		config.JSONPollItem{Key: "ghost", Path: "per_app_counts.Ghost"},
		config.JSONPollItem{Key: "nothing", Path: "nullable"},
		config.JSONPollItem{Key: "late", Path: "late_instances", Format: "int"},
	)
	doc := strings.Replace(shmStats(t), `"late_instances"`, `"nullable": null, "late_instances"`, 1)
	e := newEnv(t, cfg, body(http.StatusOK, doc))

	got, err := e.c.Collect(context.Background())

	require.Error(t, err)
	assert.Nil(t, got)
	assert.ErrorContains(t, err, `ghost: path "per_app_counts.Ghost" not found`)
	assert.ErrorContains(t, err, `nothing: path "nullable" is null`)
	assert.NotContains(t, err.Error(), "instances:")
	assert.NotContains(t, err.Error(), "late:")
	_, ok, herr := e.hist.At(context.Background(), "shm", "instances", t0)
	require.NoError(t, herr)
	assert.False(t, ok, "a failed collect leaves no history behind")
}

func TestUnknownFormat(t *testing.T) {
	e := newEnv(t, poll("x", item("v", "active_instances", "hex")), body(http.StatusOK, shmStats(t)))

	_, err := e.c.Collect(context.Background())

	require.ErrorContains(t, err, `v: unknown format "hex"`)
}

func TestBadResponses(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"unauthorized", 401, `{"error": "unauthorized"}`, "HTTP 401"},
		{"server error", 500, "boom", "HTTP 500"},
		{"html login page", 200, "<html><body>Sign in</body></html>", "decode"},
		{"empty body", 200, "", "decode"},
		{"truncated json", 200, `{"active_instances": 13`, "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, poll("x", item("v", "active_instances", "")), body(tt.status, tt.body))

			got, err := e.c.Collect(context.Background())

			require.ErrorContains(t, err, tt.wantErr)
			assert.Nil(t, got)
		})
	}
}

func TestInvalidURLDoesNotLeakSecrets(t *testing.T) {
	cfg := poll("x", item("v", "active_instances", ""))
	cfg.URL = "http://127.0.0.1/%zz?token=hunter2"
	e := newEnv(t, cfg, body(http.StatusOK, "{}"))

	_, err := e.c.Collect(context.Background())

	require.EqualError(t, err, "invalid url")
}

func TestTransportErrorDoesNotLeakSecrets(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	cfg := poll("x", item("v", "active_instances", ""))
	cfg.URL = url + "/stats?token=hunter2"
	e := newEnv(t, cfg, body(http.StatusOK, "{}"))

	_, err := e.c.Collect(context.Background())

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
	assert.NotContains(t, err.Error(), "/stats")
	assert.Contains(t, err.Error(), "refused")
}

func TestCanceledContext(t *testing.T) {
	e := newEnv(t, shmConfig(), body(http.StatusOK, shmStats(t)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := e.c.Collect(ctx)

	require.ErrorIs(t, err, context.Canceled)
}

func TestSummary(t *testing.T) {
	c := New(config.JSONPoll{Name: "x"}, collector.Deps{})
	tests := []struct {
		name string
		data any
		want string
	}{
		{"two items", Data{Name: "shm", Items: []Item{{Key: "instances", Text: "214"}, {Key: "active", Text: "131"}}}, "instances=214 active=131"},
		{"text is used rather than the value", Data{Items: []Item{{Key: "cpu", Value: 12.54, Text: "12.5%"}}}, "cpu=12.5%"},
		{"no items", Data{}, ""},
		{"foreign data", 3, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, c.Summary(tt.data))
		})
	}
}

func TestIdentity(t *testing.T) {
	c := New(config.JSONPoll{Name: "shm", Interval: 5 * time.Minute}, collector.Deps{})

	assert.Equal(t, "shm", c.Name())
	assert.Equal(t, 5*time.Minute, c.Interval())
}

func TestEveryItemKeepsItsPositionAndKey(t *testing.T) {
	var items []config.JSONPollItem
	for i := range 8 {
		items = append(items, item(fmt.Sprintf("k%d", i), "active_instances", ""))
	}
	e := newEnv(t, poll("x", items...), body(http.StatusOK, shmStats(t)))

	got := e.collect(t).Items

	require.Len(t, got, 8)
	for i, it := range got {
		assert.Equal(t, fmt.Sprintf("k%d", i), it.Key)
	}
}
