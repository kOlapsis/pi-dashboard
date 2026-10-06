package github

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/store"
)

const testToken = "ghp_test_token_0123456789"

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
}

type server struct {
	*httptest.Server
	expiry string

	mu   sync.Mutex
	reqs []seen
}

func serve(t *testing.T, routes map[string]http.HandlerFunc) *server {
	t.Helper()
	s := &server{}
	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, seen{method: r.Method, path: r.URL.Path, query: r.URL.Query(), header: r.Header.Clone()})
		s.mu.Unlock()
		if s.expiry != "" {
			w.Header().Set(expiryHeader, s.expiry)
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

func (s *server) count(pathSuffix string) int {
	n := 0
	for _, r := range s.requests() {
		if strings.HasSuffix(r.path, pathSuffix) {
			n++
		}
	}
	return n
}

func (s *server) paths() []string {
	var out []string
	for _, r := range s.requests() {
		out = append(out, r.path)
	}
	return out
}

func (s *server) requireHeaders(t *testing.T, token string) {
	t.Helper()
	reqs := s.requests()
	require.NotEmpty(t, reqs)
	for _, r := range reqs {
		want := acceptJSON
		if strings.HasSuffix(r.path, "/stargazers") {
			want = acceptStar
		}
		require.Equal(t, want, r.header.Get("Accept"), r.path)
		require.Equal(t, "2022-11-28", r.header.Get("X-GitHub-Api-Version"), r.path)
		if token == "" {
			require.Empty(t, r.header.Get("Authorization"), r.path)
		} else {
			require.Equal(t, "Bearer "+token, r.header.Get("Authorization"), r.path)
		}
	}
}

func reply(w http.ResponseWriter, status int, body []byte, headers ...string) {
	for i := 0; i+1 < len(headers); i += 2 {
		w.Header().Set(headers[i], headers[i+1])
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

var notFound = []byte(`{"message":"Not Found","documentation_url":"https://docs.github.com/rest","status":"404"}`)

func rateLimited(t *testing.T, w http.ResponseWriter, status int, reset time.Time) {
	reply(w, status, fixture(t, "rate_limited.json"),
		"X-RateLimit-Limit", "60", "X-RateLimit-Remaining", "0", "X-RateLimit-Resource", "core",
		"X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}

func timeline(n int, last time.Time, gap time.Duration) []time.Time {
	out := make([]time.Time, n)
	for i := range out {
		out[i] = last.Add(-time.Duration(n-1-i) * gap).UTC()
	}
	return out
}

func starPage(times []time.Time, q url.Values) []byte {
	per, page := atoiOr(q.Get("per_page"), 30), atoiOr(q.Get("page"), 1)
	lo := min((page-1)*per, len(times))
	hi := min(lo+per, len(times))
	items := make([]map[string]any, 0, hi-lo)
	for i := lo; i < hi; i++ {
		items = append(items, map[string]any{
			"starred_at": times[i].Format(time.RFC3339),
			"user":       map[string]any{"login": fmt.Sprintf("stargazer-%d", i), "id": 2000000 + i, "type": "User"},
		})
	}
	b, err := json.Marshal(items)
	if err != nil {
		panic(err)
	}
	return b
}

func fixtureStars(t testing.TB, name string) []time.Time {
	var items []stargazer
	require.NoError(t, json.Unmarshal(fixture(t, name), &items))
	out := make([]time.Time, len(items))
	for i, it := range items {
		out[i] = it.StarredAt
	}
	return out
}

func starTimelines(t testing.TB) map[string][]time.Time {
	last := testNow.Add(-2 * time.Hour)
	return map[string][]time.Time{
		"maintenant":    timeline(528, last, 5*time.Hour),
		"ackify":        timeline(209, last.Add(-3*24*time.Hour), 14*time.Hour),
		"shm":           timeline(166, last.Add(-9*24*time.Hour), 20*time.Hour),
		"gofact":        fixtureStars(t, "stargazers_gofact.json"),
		"speckit-guard": fixtureStars(t, "stargazers_speckit-guard.json"),
	}
}

func defaultRoutes(t *testing.T) map[string]http.HandlerFunc {
	t.Helper()
	page1, page2 := fixture(t, "org_repos_page1.json"), fixture(t, "org_repos_page2.json")
	byName := map[string][]byte{}
	for _, page := range [][]byte{page1, page2} {
		var objs []json.RawMessage
		require.NoError(t, json.Unmarshal(page, &objs))
		for _, o := range objs {
			var n struct {
				Name string `json:"name"`
			}
			require.NoError(t, json.Unmarshal(o, &n))
			byName[n.Name] = o
		}
	}
	timelines := starTimelines(t)
	rawStars := map[string][]byte{
		"gofact":        fixture(t, "stargazers_gofact.json"),
		"speckit-guard": fixture(t, "stargazers_speckit-guard.json"),
	}

	orgRepos := func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		const query = "per_page=100&type=public&sort=pushed"
		switch r.URL.Query().Get("page") {
		case "", "1":
			next := fmt.Sprintf("%s/organizations/187254031/repos?%s&page=2", base, query)
			reply(w, http.StatusOK, page1, "Link", fmt.Sprintf(`<%s>; rel="next", <%s>; rel="last"`, next, next))
		case "2":
			first := fmt.Sprintf("%s/organizations/187254031/repos?%s&page=1", base, query)
			reply(w, http.StatusOK, page2, "Link", fmt.Sprintf(`<%s>; rel="prev", <%s>; rel="first"`, first, first))
		default:
			reply(w, http.StatusOK, []byte("[]"))
		}
	}
	traffic := func(kind string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, err := os.ReadFile(filepath.Join("testdata", "traffic_"+kind+"_"+r.PathValue("repo")+".json"))
			if err != nil {
				reply(w, http.StatusNotFound, notFound)
				return
			}
			reply(w, http.StatusOK, b)
		}
	}

	return map[string]http.HandlerFunc{
		"GET /orgs/{org}/repos":         orgRepos,
		"GET /organizations/{id}/repos": orgRepos,
		"GET /repos/{owner}/{repo}": func(w http.ResponseWriter, r *http.Request) {
			b, ok := byName[r.PathValue("repo")]
			if !ok {
				reply(w, http.StatusNotFound, notFound)
				return
			}
			reply(w, http.StatusOK, b)
		},
		"GET /repos/{owner}/{repo}/pulls": func(w http.ResponseWriter, r *http.Request) {
			switch r.PathValue("repo") {
			case "maintenant":
				next := fmt.Sprintf("http://%s/repositories/702148813/pulls?state=open&per_page=1&page=2", r.Host)
				reply(w, http.StatusOK, fixture(t, "pulls_maintenant_first.json"), "Link", fmt.Sprintf(`<%s>; rel="next", <%s>; rel="last"`, next, next))
			case "shm":
				reply(w, http.StatusOK, fixture(t, "pulls_shm.json"))
			default:
				reply(w, http.StatusOK, []byte("[]"))
			}
		},
		"GET /repos/{owner}/{repo}/traffic/views":  traffic("views"),
		"GET /repos/{owner}/{repo}/traffic/clones": traffic("clones"),
		"GET /repos/{owner}/{repo}/stargazers": func(w http.ResponseWriter, r *http.Request) {
			name := r.PathValue("repo")
			if b, ok := rawStars[name]; ok {
				if atoiOr(r.URL.Query().Get("page"), 1) == 1 {
					reply(w, http.StatusOK, b)
				} else {
					reply(w, http.StatusOK, []byte("[]"))
				}
				return
			}
			times, ok := timelines[name]
			if !ok {
				reply(w, http.StatusNotFound, notFound)
				return
			}
			reply(w, http.StatusOK, starPage(times, r.URL.Query()))
		},
	}
}

func baseConfig() config.GitHub {
	return config.GitHub{Interval: 15 * time.Minute, TrafficInterval: time.Hour, Org: "kOlapsis", Token: testToken}
}

type env struct {
	srv  *server
	c    *Collector
	hist *store.Mem
	clk  *clock.Fake
	logs *syncBuffer
}

func newEnv(t *testing.T, routes map[string]http.HandlerFunc, cfg config.GitHub) *env {
	t.Helper()
	e := &env{srv: serve(t, routes), hist: store.NewMem(), clk: clock.NewFake(testNow), logs: &syncBuffer{}}
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.c = New(cfg, collector.Deps{HTTP: e.srv.Client(), Clock: e.clk, Hist: e.hist, Log: log, Loc: paris})
	e.c.base = e.srv.URL
	return e
}

func (e *env) collect(t *testing.T) Data {
	t.Helper()
	got, err := e.c.Collect(t.Context())
	require.NoError(t, err)
	d, ok := got.(Data)
	require.True(t, ok, "Collect returns a Data value")
	return d
}

func (e *env) put(t *testing.T, key string, at time.Time, v float64) {
	t.Helper()
	require.NoError(t, e.hist.Put(t.Context(), collectorName, key, at, v))
}

func (e *env) at(t *testing.T, key string, at time.Time) (float64, bool) {
	t.Helper()
	v, ok, err := e.hist.At(t.Context(), collectorName, key, at)
	require.NoError(t, err)
	return v, ok
}

func withFields(t *testing.T, raw []byte, fields map[string]any) []byte {
	t.Helper()
	var obj map[string]any
	require.NoError(t, json.Unmarshal(raw, &obj))
	for k, v := range fields {
		obj[k] = v
	}
	b, err := json.Marshal(obj)
	require.NoError(t, err)
	return b
}
