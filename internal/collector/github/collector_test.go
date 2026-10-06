package github

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/store"
)

func seedWeekAgo(t *testing.T, e *env) {
	t.Helper()
	weekAgo := testNow.Add(-week)
	for key, v := range map[string]float64{
		"stars.total": 893, "stars.maintenant": 516, "stars.ackify": 208,
		"stars.shm": 163, "stars.gofact": 5, "stars.speckit-guard": 1,
	} {
		e.put(t, key, weekAgo, v)
	}
	for d := 1; d <= 29; d++ {
		if d != 7 {
			e.put(t, "stars.total", testNow.Add(-time.Duration(d)*24*time.Hour), float64(893+2*(7-d)))
		}
	}
}

func TestCollectOrg(t *testing.T) {
	cfg := baseConfig()
	cfg.TrafficRepos = []string{"maintenant", "ackify"}
	cfg.Exclude = []string{"herald"}
	e := newEnv(t, defaultRoutes(t), cfg)
	e.srv.expiry = "2027-01-01 12:00:00 UTC"
	seedWeekAgo(t, e)

	d := e.collect(t)

	assert.Equal(t, 909, d.Stars)
	assert.Equal(t, ptr(16), d.Delta7)
	assert.Equal(t, []Repo{
		{Name: "maintenant", Stars: 528, Delta7: ptr(12), Forks: 31, Issues: 9, PRs: 2, PushedAt: time.Date(2026, 10, 7, 9, 12, 44, 0, time.UTC), Views14: ptr(2140), Clones14: ptr(338)},
		{Name: "ackify", Stars: 209, Delta7: ptr(1), Forks: 12, Issues: 3, PRs: 0, PushedAt: time.Date(2026, 8, 28, 16, 40, 2, 0, time.UTC), Views14: ptr(260), Clones14: ptr(41)},
		{Name: "shm", Stars: 166, Delta7: ptr(3), Forks: 8, Issues: 2, PRs: 1, PushedAt: time.Date(2026, 9, 28, 7, 55, 31, 0, time.UTC)},
		{Name: "gofact", Stars: 5, Delta7: ptr(0), Forks: 0, Issues: 1, PRs: 0, PushedAt: time.Date(2026, 10, 5, 18, 21, 9, 0, time.UTC)},
		{Name: "speckit-guard", Stars: 1, Delta7: ptr(0), Forks: 0, Issues: 0, PRs: 0, PushedAt: time.Date(2026, 10, 1, 10, 2, 47, 0, time.UTC)},
	}, d.Repos)
	require.NotNil(t, d.TokenExpiresAt)
	assert.True(t, d.TokenExpiresAt.Equal(time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)))

	require.Len(t, d.Series30, 30)
	assert.Equal(t, store.Point{T: testNow.Add(-29 * 24 * time.Hour).Truncate(time.Hour).UTC(), V: 849}, d.Series30[0])
	assert.Equal(t, store.Point{T: testNow.Truncate(time.Hour).UTC(), V: 909}, d.Series30[29])

	e.srv.requireHeaders(t, testToken)
	assert.Equal(t, []string{
		"/orgs/kOlapsis/repos",
		"/organizations/187254031/repos",
		"/repos/kOlapsis/maintenant/pulls",
		"/repos/kOlapsis/maintenant/traffic/views",
		"/repos/kOlapsis/maintenant/traffic/clones",
		"/repos/kOlapsis/ackify/pulls",
		"/repos/kOlapsis/ackify/traffic/views",
		"/repos/kOlapsis/ackify/traffic/clones",
		"/repos/kOlapsis/shm/pulls",
		"/repos/kOlapsis/gofact/pulls",
	}, e.srv.paths())
	reqs := e.srv.requests()
	assert.Equal(t, "100", reqs[0].query.Get("per_page"))
	assert.Equal(t, "public", reqs[0].query.Get("type"))
	assert.Equal(t, "pushed", reqs[0].query.Get("sort"))
	assert.Equal(t, "open", reqs[2].query.Get("state"))
	assert.Equal(t, "1", reqs[2].query.Get("per_page"))

	golden(t, "collect.golden.json", d)
}

func TestCollectRecordsHistory(t *testing.T) {
	cfg := baseConfig()
	cfg.Exclude = []string{"herald"}
	e := newEnv(t, defaultRoutes(t), cfg)
	seedWeekAgo(t, e)

	e.collect(t)

	for key, want := range map[string]float64{
		"stars.total": 909, "stars.maintenant": 528, "stars.ackify": 209,
		"stars.shm": 166, "stars.gofact": 5, "stars.speckit-guard": 1,
	} {
		got, ok := e.at(t, key, testNow)
		require.True(t, ok, key)
		assert.Equal(t, want, got, key)
	}
	_, ok := e.at(t, "stars.herald", testNow)
	assert.False(t, ok)
}

func TestCollectNamedRepos(t *testing.T) {
	routes := defaultRoutes(t)
	var page []json.RawMessage
	require.NoError(t, json.Unmarshal(fixture(t, "org_repos_page1.json"), &page))
	relic := withFields(t, page[0], map[string]any{"name": "relic", "stargazers_count": 77, "forks_count": 3, "open_issues_count": 0})
	byName := routes["GET /repos/{owner}/{repo}"]
	routes["GET /repos/{owner}/{repo}"] = func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("owner") == "ghost" {
			reply(w, http.StatusOK, relic)
			return
		}
		byName(w, r)
	}
	cfg := baseConfig()
	cfg.Repos = []string{"maintenant", " kOlapsis/ackify ", "ghost/relic", "gofact"}
	cfg.Exclude = []string{"GoFact"}
	e := newEnv(t, routes, cfg)
	seedWeekAgo(t, e)

	d := e.collect(t)

	names := make([]string, len(d.Repos))
	for i, r := range d.Repos {
		names[i] = r.Name
	}
	assert.Equal(t, []string{"maintenant", "ackify", "ghost/relic"}, names)
	assert.Equal(t, 528+209+77, d.Stars)
	assert.Equal(t, []string{
		"/repos/kOlapsis/maintenant",
		"/repos/kOlapsis/ackify",
		"/repos/ghost/relic",
		"/repos/kOlapsis/maintenant/pulls",
		"/repos/kOlapsis/ackify/pulls",
	}, e.srv.paths()[:5])
	_, ok := e.at(t, "stars.ghost/relic", testNow)
	assert.True(t, ok)
	assert.Zero(t, e.srv.count("/orgs/kOlapsis/repos"))
	assert.Zero(t, e.srv.count("/repos/kOlapsis/gofact"))
}

func TestCollectNamedRepoErrors(t *testing.T) {
	tests := []struct {
		name    string
		org     string
		repos   []string
		wantErr string
		calls   int
	}{
		{name: "bare name without org", org: "", repos: []string{"lonely"}, wantErr: `"lonely"`},
		{name: "empty owner", org: "kOlapsis", repos: []string{"/name"}, wantErr: `invalid entry "/name"`},
		{name: "empty name", org: "kOlapsis", repos: []string{"owner/"}, wantErr: `invalid entry "owner/"`},
		{name: "unknown repository", org: "kOlapsis", repos: []string{"missing"}, wantErr: "get repository kOlapsis/missing: ", calls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Org, cfg.Repos = tt.org, tt.repos
			e := newEnv(t, defaultRoutes(t), cfg)

			_, err := e.c.Collect(t.Context())

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Len(t, e.srv.requests(), tt.calls)
		})
	}
}

func TestCollectIssuesNeverNegative(t *testing.T) {
	routes := defaultRoutes(t)
	var page []json.RawMessage
	require.NoError(t, json.Unmarshal(fixture(t, "org_repos_page1.json"), &page))
	skewed := withFields(t, page[0], map[string]any{"name": "skewed", "stargazers_count": 3, "open_issues_count": 1})
	routes["GET /repos/{owner}/{repo}"] = func(w http.ResponseWriter, _ *http.Request) { reply(w, http.StatusOK, skewed) }
	routes["GET /repos/{owner}/{repo}/pulls"] = func(w http.ResponseWriter, r *http.Request) {
		last := "http://" + r.Host + "/repositories/1/pulls?state=open&per_page=1&page=3"
		reply(w, http.StatusOK, []byte(`[{"number":9}]`), "Link", `<`+last+`>; rel="next", <`+last+`>; rel="last"`)
	}
	cfg := baseConfig()
	cfg.Repos = []string{"skewed"}
	e := newEnv(t, routes, cfg)
	e.put(t, keyTotal, testNow.Add(-week), 1)

	d := e.collect(t)

	require.Len(t, d.Repos, 1)
	assert.Equal(t, 3, d.Repos[0].PRs)
	assert.Equal(t, 0, d.Repos[0].Issues)
}

func TestCollectRepositoryNeverPushedTo(t *testing.T) {
	routes := defaultRoutes(t)
	var page []json.RawMessage
	require.NoError(t, json.Unmarshal(fixture(t, "org_repos_page1.json"), &page))
	fresh := withFields(t, page[0], map[string]any{
		"name": "fresh", "stargazers_count": 0, "open_issues_count": 0, "pushed_at": nil, "created_at": "2026-10-06T08:00:00Z",
	})
	routes["GET /repos/{owner}/{repo}"] = func(w http.ResponseWriter, _ *http.Request) { reply(w, http.StatusOK, fresh) }
	cfg := baseConfig()
	cfg.Repos = []string{"fresh"}
	e := newEnv(t, routes, cfg)

	d := e.collect(t)

	require.Len(t, d.Repos, 1)
	assert.Equal(t, time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC), d.Repos[0].PushedAt, "falls back to the creation date")
	assert.Zero(t, e.srv.count("/stargazers"), "no stargazer to fetch")
	got, ok := e.at(t, "stars.fresh", localMidnights(testNow, 1)[0])
	assert.True(t, ok)
	assert.Zero(t, got)
}

func TestCollectWithoutHistory(t *testing.T) {
	srv := serve(t, defaultRoutes(t))
	cfg := baseConfig()
	cfg.Token = ""
	c := New(cfg, collector.Deps{HTTP: srv.Client(), Clock: clock.NewFake(testNow), Hist: nil, Log: nil, Loc: paris})
	c.base = srv.URL

	got, err := c.Collect(t.Context())

	require.NoError(t, err)
	d := got.(Data)
	assert.Equal(t, 923, d.Stars)
	assert.Nil(t, d.Delta7)
	for _, r := range d.Repos {
		assert.Nil(t, r.Delta7, r.Name)
	}
	assert.NotNil(t, d.Series30)
	assert.Empty(t, d.Series30)
	assert.Nil(t, d.TokenExpiresAt)
	assert.Zero(t, srv.count("/stargazers"))
	srv.requireHeaders(t, "")
	b, err := json.Marshal(d)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"series30":[]`)
	assert.NotContains(t, string(b), "token_expires_at")
}

func TestCollectEmptyOrg(t *testing.T) {
	routes := defaultRoutes(t)
	routes["GET /orgs/{org}/repos"] = func(w http.ResponseWriter, _ *http.Request) { reply(w, http.StatusOK, []byte("[]")) }
	e := newEnv(t, routes, baseConfig())

	d := e.collect(t)

	assert.Zero(t, d.Stars)
	assert.NotNil(t, d.Repos)
	assert.Empty(t, d.Repos)
	b, err := json.Marshal(d)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"repos":[]`)
}

func TestCollectConcurrent(t *testing.T) {
	cfg := baseConfig()
	cfg.TrafficRepos = []string{"maintenant"}
	e := newEnv(t, defaultRoutes(t), cfg)
	seedWeekAgo(t, e)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.c.Collect(t.Context())
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
}

func TestSummary(t *testing.T) {
	c := New(baseConfig(), collector.Deps{})
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"delta and repos", Data{Stars: 909, Delta7: ptr(16), Repos: make([]Repo, 5)}, "909 ★ (+16 / 7 j) · 5 repos"},
		{"thousands separator", Data{Stars: 1204, Delta7: ptr(-3), Repos: make([]Repo, 1)}, "1 204 ★ (-3 / 7 j) · 1 repo"},
		{"no history", Data{Stars: 12}, "12 ★ · 0 repo"},
		{"zero delta", Data{Stars: 0, Delta7: ptr(0), Repos: make([]Repo, 2)}, "0 ★ (+0 / 7 j) · 2 repos"},
		{"foreign value", "oops", ""},
		{"nil", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, c.Summary(tt.in))
		})
	}
}

func TestMetadata(t *testing.T) {
	cfg := baseConfig()
	c := New(cfg, collector.Deps{})

	assert.Equal(t, "github", c.Name())
	assert.Equal(t, 15*time.Minute, c.Interval())
	assert.Equal(t, 90*time.Second, c.Timeout())
	var _ collector.Collector = c
	var _ collector.Summarizer = c
	var _ collector.Timeouter = c
}

func TestListed(t *testing.T) {
	tests := []struct {
		name    string
		entries []string
		want    bool
	}{
		{"bare name", []string{"herald"}, true},
		{"case insensitive", []string{"HERALD"}, true},
		{"owner and name", []string{"kolapsis/Herald"}, true},
		{"padded", []string{"  herald "}, true},
		{"other owner", []string{"someone/herald"}, false},
		{"other name", []string{"heral"}, false},
		{"empty", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, listed(tt.entries, "kOlapsis", "herald"))
		})
	}
}
