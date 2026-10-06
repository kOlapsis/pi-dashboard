package umami

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
)

const (
	siteA = "95bc46c9-6c87-45c6-9e8b-5a8f95889191"
	siteB = "3f0c1c52-8c4e-4b0b-9c0e-6f7d8f0a2b11"
	siteC = "b7d2e8aa-1f5c-4d58-8a47-0c1f2d3e4a55"

	testUser = "admin"
	testPass = "s3cret-test-password"
)

var (
	update  = flag.Bool("update", false, "rewrite golden files")
	paris   = mustLocation("Europe/Paris")
	testNow = time.Date(2026, 10, 7, 14, 5, 0, 0, paris)
)

func mustLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func ptr[T any](v T) *T { return &v }

func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return b
}

func golden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	require.NoError(t, err)
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.WriteFile(path, append(got, '\n'), 0o644))
	}
	require.JSONEq(t, string(fixture(t, name)), string(got))
}

func reply(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

var unauthorized = []byte(`{"error":"Unauthorized"}`)

type syncBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

type seen struct {
	method, path string
	query        url.Values
	header       http.Header
	body         string
}

type server struct {
	*httptest.Server
	delay time.Duration

	mu          sync.Mutex
	reqs        []seen
	inflight    int
	maxInflight int
}

func serve(t *testing.T, routes map[string]http.HandlerFunc) *server {
	t.Helper()
	s := &server{}
	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		s.mu.Lock()
		s.reqs = append(s.reqs, seen{method: r.Method, path: r.URL.Path, query: r.URL.Query(), header: r.Header.Clone(), body: string(raw)})
		s.inflight++
		s.maxInflight = max(s.maxInflight, s.inflight)
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.inflight--
			s.mu.Unlock()
		}()
		if s.delay > 0 {
			time.Sleep(s.delay)
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) requests() []seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seen(nil), s.reqs...)
}

func (s *server) count(method, path string) int {
	n := 0
	for _, r := range s.requests() {
		if r.method == method && r.path == path {
			n++
		}
	}
	return n
}

func (s *server) concurrency() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxInflight
}

type auth struct {
	apiKey string

	mu       sync.Mutex
	valid    map[string]bool
	logins   int
	attempts int
}

func newAuth() *auth { return &auth{valid: map[string]bool{}} }

func (a *auth) ok(r *http.Request) bool {
	tok, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.apiKey != "" {
		return tok == a.apiKey
	}
	return a.valid[tok]
}

func (a *auth) revokeAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.valid = map[string]bool{}
}

func (a *auth) loginCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.logins
}

func (a *auth) attemptCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.attempts
}

func (a *auth) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.ok(r) {
			reply(w, http.StatusUnauthorized, unauthorized)
			return
		}
		h(w, r)
	}
}

func (a *auth) login(t *testing.T) http.HandlerFunc {
	user := fixture(t, "login_user.json")
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			reply(w, http.StatusBadRequest, []byte(`{"error":"bad body"}`))
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.attempts++
		if in.Username != testUser || in.Password != testPass {
			reply(w, http.StatusUnauthorized, []byte(`{"error":"Incorrect username and/or password"}`))
			return
		}
		a.logins++
		tok := fmt.Sprintf("eyJhbGciOiJIUzI1NiJ9.e30.token%d", a.logins)
		a.valid[tok] = true
		body, err := json.Marshal(map[string]any{"token": tok, "user": json.RawMessage(user)})
		if err != nil {
			panic(err)
		}
		reply(w, http.StatusOK, body)
	}
}

type perSite map[string]json.RawMessage

func loadPerSite(t testing.TB, name string) map[string]perSite {
	var out map[string]perSite
	require.NoError(t, json.Unmarshal(fixture(t, name), &out))
	return out
}

func loadSimple(t testing.TB, name string) perSite {
	var out perSite
	require.NoError(t, json.Unmarshal(fixture(t, name), &out))
	return out
}

func routesFor(t *testing.T, a *auth, version, list string) map[string]http.HandlerFunc {
	t.Helper()
	stats := loadPerSite(t, version+"_stats.json")
	pageviews := loadSimple(t, version+"_pageviews.json")
	active := loadSimple(t, version+"_active.json")
	listBody := fixture(t, list)
	serveSite := func(source perSite) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, ok := source[r.PathValue("id")]
			if !ok {
				reply(w, http.StatusNotFound, []byte(`{"error":"Website not found"}`))
				return
			}
			reply(w, http.StatusOK, b)
		}
	}
	return map[string]http.HandlerFunc{
		"POST /api/auth/login": a.login(t),
		"GET /api/websites":    a.guard(func(w http.ResponseWriter, _ *http.Request) { reply(w, http.StatusOK, listBody) }),
		"GET /api/websites/{id}/stats": a.guard(func(w http.ResponseWriter, r *http.Request) {
			window := "prev"
			if r.URL.Query().Get("endAt") == fmt.Sprint(testNow.UnixMilli()) {
				window = "cur"
			}
			b, ok := stats[r.PathValue("id")][window]
			if !ok {
				reply(w, http.StatusNotFound, []byte(`{"error":"Website not found"}`))
				return
			}
			reply(w, http.StatusOK, b)
		}),
		"GET /api/websites/{id}/pageviews": a.guard(serveSite(pageviews)),
		"GET /api/websites/{id}/active":    a.guard(serveSite(active)),
	}
}

func baseConfig() config.Umami {
	return config.Umami{Interval: 2 * time.Minute, Username: testUser, Password: testPass}
}

type env struct {
	srv  *server
	auth *auth
	c    *Collector
	clk  *clock.Fake
	logs *syncBuffer
}

func newEnv(t *testing.T, a *auth, routes map[string]http.HandlerFunc, cfg config.Umami) *env {
	t.Helper()
	e := &env{srv: serve(t, routes), auth: a, clk: clock.NewFake(testNow), logs: &syncBuffer{}}
	cfg.BaseURL = e.srv.URL + "/"
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.c = New(cfg, collector.Deps{HTTP: e.srv.Client(), Clock: e.clk, Log: log, Loc: paris})
	return e
}

func defaultEnv(t *testing.T, version string, cfg config.Umami) *env {
	t.Helper()
	a := newAuth()
	return newEnv(t, a, routesFor(t, a, version, "websites_data.json"), cfg)
}

func (e *env) collect(t *testing.T) Data {
	t.Helper()
	got, err := e.c.Collect(t.Context())
	require.NoError(t, err)
	d, ok := got.(Data)
	require.True(t, ok, "Collect returns a Data value")
	return d
}

func siteNames(d Data) []string {
	out := make([]string, len(d.Sites))
	for i, s := range d.Sites {
		out[i] = s.Name
	}
	return out
}
