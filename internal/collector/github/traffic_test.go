package github

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func trafficEnv(t *testing.T, routes map[string]http.HandlerFunc) *env {
	t.Helper()
	cfg := baseConfig()
	cfg.TrafficRepos = []string{"maintenant"}
	cfg.Repos = []string{"maintenant"}
	e := newEnv(t, routes, cfg)
	e.put(t, keyTotal, testNow.Add(-week), 500)
	return e
}

func maintenant(t *testing.T, d Data) Repo {
	t.Helper()
	require.Len(t, d.Repos, 1)
	return d.Repos[0]
}

func countRoute(views, clones *atomic.Int64) (http.HandlerFunc, http.HandlerFunc) {
	render := func(n *atomic.Int64, key string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			reply(w, http.StatusOK, fmt.Appendf(nil, `{"count":%d,"uniques":1,%q:[]}`, n.Load(), key))
		}
	}
	return render(views, "views"), render(clones, "clones")
}

func TestTrafficIsCachedForTheTrafficInterval(t *testing.T) {
	var views, clones atomic.Int64
	views.Store(2140)
	clones.Store(338)
	routes := defaultRoutes(t)
	routes["GET /repos/{owner}/{repo}/traffic/views"], routes["GET /repos/{owner}/{repo}/traffic/clones"] = countRoute(&views, &clones)
	e := trafficEnv(t, routes)

	first := maintenant(t, e.collect(t))
	assert.Equal(t, ptr(2140), first.Views14)
	assert.Equal(t, ptr(338), first.Clones14)

	views.Store(2200)
	clones.Store(350)
	e.clk.Advance(30 * time.Minute)
	cached := maintenant(t, e.collect(t))
	assert.Equal(t, ptr(2140), cached.Views14)
	assert.Equal(t, ptr(338), cached.Clones14)
	assert.Equal(t, 1, e.srv.count("/traffic/views"))
	assert.Equal(t, 1, e.srv.count("/traffic/clones"))

	e.clk.Advance(31 * time.Minute)
	fresh := maintenant(t, e.collect(t))
	assert.Equal(t, ptr(2200), fresh.Views14)
	assert.Equal(t, ptr(350), fresh.Clones14)
	assert.Equal(t, 2, e.srv.count("/traffic/views"))
	assert.Equal(t, 2, e.srv.count("/traffic/clones"))
}

func TestTrafficIsOnlyFetchedForListedRepos(t *testing.T) {
	cfg := baseConfig()
	cfg.TrafficRepos = []string{"KOLAPSIS/Ackify"}
	cfg.Exclude = []string{"herald"}
	e := newEnv(t, defaultRoutes(t), cfg)
	e.put(t, keyTotal, testNow.Add(-week), 500)

	d := e.collect(t)

	for _, r := range d.Repos {
		if r.Name == "ackify" {
			assert.Equal(t, ptr(260), r.Views14)
			assert.Equal(t, ptr(41), r.Clones14)
			continue
		}
		assert.Nil(t, r.Views14, r.Name)
		assert.Nil(t, r.Clones14, r.Name)
	}
	assert.Equal(t, 1, e.srv.count("/traffic/views"))
	assert.Equal(t, 1, e.srv.count("/traffic/clones"))
}

func TestTrafficForbiddenWarnsOnceAndKeepsCollecting(t *testing.T) {
	var forbidden atomic.Bool
	forbidden.Store(true)
	routes := defaultRoutes(t)
	views := routes["GET /repos/{owner}/{repo}/traffic/views"]
	routes["GET /repos/{owner}/{repo}/traffic/views"] = func(w http.ResponseWriter, r *http.Request) {
		if forbidden.Load() {
			reply(w, http.StatusForbidden, fixture(t, "forbidden_traffic.json"))
			return
		}
		views(w, r)
	}
	e := trafficEnv(t, routes)
	warned := func() int { return strings.Count(e.logs.String(), "github traffic unavailable") }

	r := maintenant(t, e.collect(t))
	assert.Nil(t, r.Views14)
	assert.Nil(t, r.Clones14)
	assert.Equal(t, 1, warned())
	assert.Equal(t, 1, e.srv.count("/traffic/views"))
	assert.Zero(t, e.srv.count("/traffic/clones"))
	assert.Contains(t, e.logs.String(), "Administration: read")
	assert.Equal(t, 528, r.Stars)

	e.clk.Advance(30 * time.Minute)
	maintenant(t, e.collect(t))
	assert.Equal(t, 1, e.srv.count("/traffic/views"), "not retried before the traffic interval")

	e.clk.Advance(31 * time.Minute)
	r = maintenant(t, e.collect(t))
	assert.Nil(t, r.Views14)
	assert.Equal(t, 2, e.srv.count("/traffic/views"))
	assert.Equal(t, 1, warned(), "the warning is logged once")

	forbidden.Store(false)
	e.clk.Advance(61 * time.Minute)
	r = maintenant(t, e.collect(t))
	assert.Equal(t, ptr(2140), r.Views14, "the token permission can be fixed without a restart")

	forbidden.Store(true)
	e.clk.Advance(61 * time.Minute)
	r = maintenant(t, e.collect(t))
	assert.Nil(t, r.Views14)
	assert.Equal(t, 2, warned(), "a new denial after a success warns again")
}

func TestTrafficTransientFailureKeepsTheCachedValues(t *testing.T) {
	var failing atomic.Bool
	failing.Store(true)
	routes := defaultRoutes(t)
	views := routes["GET /repos/{owner}/{repo}/traffic/views"]
	routes["GET /repos/{owner}/{repo}/traffic/views"] = func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			reply(w, http.StatusBadGateway, []byte(`{"message":"Server Error"}`))
			return
		}
		views(w, r)
	}
	e := trafficEnv(t, routes)

	r := maintenant(t, e.collect(t))
	assert.Nil(t, r.Views14, "no cached value yet")
	assert.Contains(t, e.logs.String(), "github traffic failed")
	assert.NotContains(t, e.logs.String(), "github traffic unavailable")

	failing.Store(false)
	r = maintenant(t, e.collect(t))
	assert.Equal(t, ptr(2140), r.Views14, "a failed attempt is retried at the next collect")
	assert.Equal(t, ptr(338), r.Clones14)

	failing.Store(true)
	e.clk.Advance(61 * time.Minute)
	r = maintenant(t, e.collect(t))
	assert.Equal(t, ptr(2140), r.Views14, "the last good value survives a failure")
	assert.Equal(t, 3, e.srv.count("/traffic/views"))
}

func TestTrafficRateLimitFailsTheCollect(t *testing.T) {
	routes := defaultRoutes(t)
	routes["GET /repos/{owner}/{repo}/traffic/views"] = func(w http.ResponseWriter, _ *http.Request) {
		rateLimited(t, w, http.StatusForbidden, time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC))
	}
	e := trafficEnv(t, routes)

	_, err := e.c.Collect(t.Context())

	require.Error(t, err)
	var rl *rateLimitError
	require.True(t, errors.As(err, &rl))
	assert.Contains(t, err.Error(), "resets at 14:30")
	assert.NotContains(t, e.logs.String(), "github traffic unavailable")
}
