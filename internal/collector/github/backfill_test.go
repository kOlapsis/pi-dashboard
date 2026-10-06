package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/collector"
)

func localMidnights(now time.Time, n int) []time.Time {
	y, m, d := now.In(paris).Date()
	out := make([]time.Time, n)
	for i := range out {
		out[i] = time.Date(y, m, d-(n-1-i), 0, 0, 0, 0, paris)
	}
	return out
}

func starsAt(times []time.Time, boundary time.Time) int {
	n := 0
	for _, ts := range times {
		if !ts.After(boundary) {
			n++
		}
	}
	return n
}

func TestBackfillRebuildsNinetyDaysOfStars(t *testing.T) {
	cfg := baseConfig()
	cfg.Exclude = []string{"herald"}
	e := newEnv(t, defaultRoutes(t), cfg)
	timelines := starTimelines(t)

	d := e.collect(t)

	assert.Equal(t, 13, e.srv.count("/stargazers"), "6 + 3 + 2 + 1 + 1 pages, no request past a short page")
	for repo, pages := range map[string]int{"maintenant": 6, "ackify": 3, "shm": 2, "gofact": 1, "speckit-guard": 1} {
		got := 0
		for _, r := range e.srv.requests() {
			if r.path == "/repos/kOlapsis/"+repo+"/stargazers" {
				got++
				assert.Equal(t, "100", r.query.Get("per_page"))
			}
		}
		assert.Equal(t, pages, got, repo)
	}
	assert.Contains(t, e.logs.String(), "pages=13")
	e.srv.requireHeaders(t, testToken)

	days := localMidnights(testNow, 90)
	for i, day := range days {
		total := 0
		for repo, times := range timelines {
			want := starsAt(times, day)
			total += want
			got, ok := e.at(t, "stars."+repo, day)
			require.True(t, ok, "%s %s", repo, day)
			require.Equal(t, float64(want), got, "stars.%s at %s (day %d)", repo, day.Format("2006-01-02"), i)
		}
		got, ok := e.at(t, "stars.total", day)
		require.True(t, ok)
		require.Equal(t, float64(total), got, "stars.total at %s", day.Format("2006-01-02"))
	}

	require.NotNil(t, d.Delta7)
	weekAgo := localMidnights(testNow.Add(-week), 1)[0]
	want := 0
	for _, times := range timelines {
		want += starsAt(times, weekAgo)
	}
	assert.Equal(t, 909-want, *d.Delta7)
	assert.Len(t, d.Series30, 31, "30 local midnights and the live point")
	for _, r := range d.Repos {
		assert.NotNil(t, r.Delta7, r.Name)
	}
}

func TestBackfillBoundaryIsInclusive(t *testing.T) {
	cfg := baseConfig()
	cfg.Repos = []string{"gofact"}
	e := newEnv(t, defaultRoutes(t), cfg)

	e.collect(t)

	midnight := time.Date(2026, 10, 6, 0, 0, 0, 0, paris)
	assert.True(t, midnight.Equal(time.Date(2026, 10, 5, 22, 0, 0, 0, time.UTC)))
	for day, want := range map[time.Time]float64{
		time.Date(2026, 10, 5, 0, 0, 0, 0, paris): 3,
		midnight: 4,
		time.Date(2026, 10, 7, 0, 0, 0, 0, paris): 5,
	} {
		got, ok := e.at(t, "stars.gofact", day)
		require.True(t, ok)
		assert.Equal(t, want, got, day.Format("2006-01-02"))
	}
}

func TestBackfillRunsOncePerProcess(t *testing.T) {
	cfg := baseConfig()
	cfg.Exclude = []string{"herald"}
	e := newEnv(t, defaultRoutes(t), cfg)

	e.collect(t)
	first := e.srv.count("/stargazers")
	e.clk.Advance(15 * time.Minute)
	e.collect(t)
	e.clk.Advance(15 * time.Minute)
	e.collect(t)

	assert.Positive(t, first)
	assert.Equal(t, first, e.srv.count("/stargazers"))
}

func TestBackfillIsSkippedWhenHistoryExists(t *testing.T) {
	e := newEnv(t, defaultRoutes(t), baseConfig())
	seedWeekAgo(t, e)

	d := e.collect(t)

	assert.Zero(t, e.srv.count("/stargazers"))
	require.NotNil(t, d.Delta7)
	assert.Equal(t, 923-893, *d.Delta7)
}

func TestBackfillFailureIsRetriedAtTheNextCollect(t *testing.T) {
	var calls atomic.Int64
	routes := defaultRoutes(t)
	stargazers := routes["GET /repos/{owner}/{repo}/stargazers"]
	routes["GET /repos/{owner}/{repo}/stargazers"] = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 2 {
			reply(w, http.StatusBadGateway, []byte(`{"message":"Server Error"}`))
			return
		}
		stargazers(w, r)
	}
	cfg := baseConfig()
	cfg.Exclude = []string{"herald"}
	e := newEnv(t, routes, cfg)

	d := e.collect(t)

	assert.Contains(t, e.logs.String(), "github star history backfill failed")
	assert.Equal(t, 909, d.Stars, "the collect still succeeds")
	assert.Nil(t, d.Delta7)
	assert.Len(t, d.Series30, 1, "nothing partial is recorded")
	_, ok := e.at(t, "stars.maintenant", testNow.Add(-30*24*time.Hour))
	assert.False(t, ok)

	e.clk.Advance(15 * time.Minute)
	d = e.collect(t)

	require.NotNil(t, d.Delta7)
	assert.Len(t, d.Series30, 31)
	_, ok = e.at(t, "stars.total", testNow.Add(-30*24*time.Hour))
	assert.True(t, ok)
}

func TestBackfillNeedsAToken(t *testing.T) {
	cfg := baseConfig()
	cfg.Token = ""
	e := newEnv(t, defaultRoutes(t), cfg)

	d := e.collect(t)
	e.clk.Advance(15 * time.Minute)
	e.collect(t)

	assert.Equal(t, 923, d.Stars)
	assert.Nil(t, d.Delta7)
	assert.Zero(t, e.srv.count("/stargazers"), "the stargazers endpoint answers 401 without a token")
	assert.Equal(t, 1, strings.Count(e.logs.String(), "needs a token"))
}

func TestBackfillGivesUpAfterThreeAttempts(t *testing.T) {
	routes := defaultRoutes(t)
	routes["GET /repos/{owner}/{repo}/stargazers"] = func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusBadGateway, []byte(`{"message":"Server Error"}`))
	}
	e := newEnv(t, routes, baseConfig())

	for range 5 {
		e.collect(t)
		e.clk.Advance(15 * time.Minute)
	}

	assert.Equal(t, 3, e.srv.count("/stargazers"))
	assert.Equal(t, 3, strings.Count(e.logs.String(), "star history backfill failed"))
	assert.Contains(t, e.logs.String(), "attempt=3 of=3")
}

func TestBackfillSkipsTheTotalWhenARepoIsOverTheCap(t *testing.T) {
	routes := defaultRoutes(t)
	var page []json.RawMessage
	require.NoError(t, json.Unmarshal(fixture(t, "org_repos_page1.json"), &page))
	big := withFields(t, page[0], map[string]any{"name": "big", "stargazers_count": maxBackfillStars + 1, "open_issues_count": 0})
	byName := routes["GET /repos/{owner}/{repo}"]
	routes["GET /repos/{owner}/{repo}"] = func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("repo") == "big" {
			reply(w, http.StatusOK, big)
			return
		}
		byName(w, r)
	}
	cfg := baseConfig()
	cfg.Repos = []string{"big", "gofact"}
	e := newEnv(t, routes, cfg)

	d := e.collect(t)

	assert.Equal(t, 1, e.srv.count("/stargazers"), "only gofact is paged")
	assert.Zero(t, e.srv.count("/repos/kOlapsis/big/stargazers"))
	_, ok := e.at(t, "stars.gofact", testNow.Add(-30*24*time.Hour))
	assert.True(t, ok, "other repositories are still backfilled")
	_, ok = e.at(t, "stars.total", testNow.Add(-30*24*time.Hour))
	assert.False(t, ok, "a total without the big repository would be wrong")
	assert.Nil(t, d.Delta7)
	assert.Contains(t, e.logs.String(), "not backfilled for the total")

	e.clk.Advance(15 * time.Minute)
	e.collect(t)
	assert.Equal(t, 1, e.srv.count("/stargazers"), "not retried")
}

func TestBackfillStopsAtFiftyPages(t *testing.T) {
	routes := defaultRoutes(t)
	var page []json.RawMessage
	require.NoError(t, json.Unmarshal(fixture(t, "org_repos_page1.json"), &page))
	huge := withFields(t, page[0], map[string]any{"name": "huge", "stargazers_count": maxBackfillStars, "open_issues_count": 0})
	times := timeline(maxBackfillStars, testNow.Add(-time.Hour), 30*time.Minute)
	routes["GET /repos/{owner}/{repo}"] = func(w http.ResponseWriter, _ *http.Request) { reply(w, http.StatusOK, huge) }
	routes["GET /repos/{owner}/{repo}/stargazers"] = func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, starPage(times, r.URL.Query()))
	}
	cfg := baseConfig()
	cfg.Repos = []string{"huge"}
	e := newEnv(t, routes, cfg)

	e.collect(t)

	assert.Equal(t, 50, e.srv.count("/stargazers"))
	day := localMidnights(testNow, 90)[40]
	got, ok := e.at(t, "stars.huge", day)
	require.True(t, ok)
	assert.Equal(t, float64(starsAt(times, day)), got)
}

func TestBackfillLeavesTheRateLimitErrorToTheNextCollect(t *testing.T) {
	routes := defaultRoutes(t)
	routes["GET /repos/{owner}/{repo}/stargazers"] = func(w http.ResponseWriter, _ *http.Request) {
		rateLimited(t, w, http.StatusForbidden, time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC))
	}
	e := newEnv(t, routes, baseConfig())

	d := e.collect(t)

	assert.Equal(t, 923, d.Stars)
	assert.Contains(t, e.logs.String(), "resets at 14:30")
	assert.True(t, strings.Contains(e.logs.String(), "backfill failed"))
	assert.Equal(t, 1, e.srv.count("/stargazers"), "stops at the first limited call")
}

func TestCumulative(t *testing.T) {
	at := func(day, hour int) time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, time.UTC) }
	days := []time.Time{at(1, 0), at(2, 0), at(3, 0), at(4, 0)}
	tests := []struct {
		name  string
		times []time.Time
		want  []int
	}{
		{"no stars", nil, []int{0, 0, 0, 0}},
		{"stars before the first day", []time.Time{at(1, 0).Add(-time.Hour), at(1, 0).Add(-time.Minute)}, []int{2, 2, 2, 2}},
		{"star exactly on a boundary counts", []time.Time{at(2, 0)}, []int{0, 1, 1, 1}},
		{"several in one day", []time.Time{at(2, 1), at(2, 9), at(2, 23), at(3, 5)}, []int{0, 0, 3, 4}},
		{"stars after the last day", []time.Time{at(4, 1)}, []int{0, 0, 0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, cumulative(tt.times, days))
		})
	}
}

func TestMidnightsAcrossDST(t *testing.T) {
	c := New(baseConfig(), collector.Deps{Loc: paris})
	now := time.Date(2026, 10, 26, 0, 30, 0, 0, paris)

	days := c.midnights(now)

	require.Len(t, days, backfillDays)
	assert.Equal(t, "2026-10-26 00:00:00 +0100", days[89].Format("2006-01-02 15:04:05 -0700"))
	assert.Equal(t, "2026-10-25 00:00:00 +0200", days[88].Format("2006-01-02 15:04:05 -0700"))
	assert.Equal(t, "2026-10-24 00:00:00 +0200", days[87].Format("2006-01-02 15:04:05 -0700"))
	for i := 1; i < len(days); i++ {
		require.Equal(t, time.Date(days[i-1].Year(), days[i-1].Month(), days[i-1].Day()+1, 0, 0, 0, 0, paris), days[i], fmt.Sprint(i))
	}
}
