package umami

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
)

var days14 = []string{
	"2026-09-24", "2026-09-25", "2026-09-26", "2026-09-27", "2026-09-28", "2026-09-29", "2026-09-30",
	"2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04", "2026-10-05", "2026-10-06", "2026-10-07",
}

func dayPoints(days []string, values ...float64) []collector.DayPoint {
	out := make([]collector.DayPoint, len(days))
	for i, d := range days {
		out[i] = collector.DayPoint{D: d, V: values[i]}
	}
	return out
}

func wantData() Data {
	return Data{
		Visits7: 1148, Prev7: 1067, DeltaPct: ptr(7.6), Live: 3,
		Bars14: dayPoints(days14, 153, 140, 53, 42, 165, 183, 168, 155, 177, 61, 49, 180, 191, 88),
		Sites: []Site{
			{
				ID: siteA, Name: "maintenant.dev", Visits7: 812, Prev7: 745, Pageviews7: 2210, DeltaPct: ptr(9.0), Live: 2,
				Bars14: dayPoints(days14, 104, 98, 41, 33, 112, 125, 118, 109, 121, 47, 38, 128, 133, 61),
			},
			{
				ID: siteB, Name: "restoreproof.io", Visits7: 240, Prev7: 251, Pageviews7: 610, DeltaPct: ptr(-4.4), Live: 1,
				Bars14: dayPoints(days14, 35, 31, 12, 9, 38, 41, 36, 33, 40, 14, 11, 37, 42, 20),
			},
			{
				ID: siteC, Name: "kolapsis.com", Visits7: 96, Prev7: 71, Pageviews7: 180, DeltaPct: ptr(35.2), Live: 0,
				Bars14: dayPoints(days14, 14, 11, 0, 0, 15, 17, 14, 13, 16, 0, 0, 15, 16, 7),
			},
		},
	}
}

func TestCollect(t *testing.T) {
	tests := []struct{ name, version, list string }{
		{"v2 shapes, data envelope", "v2", "websites_data.json"},
		{"v3 shapes, data envelope", "v3", "websites_data.json"},
		{"v2 shapes, bare array", "v2", "websites_array.json"},
		{"v3 shapes, bare array", "v3", "websites_array.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAuth()
			e := newEnv(t, a, routesFor(t, a, tt.version, tt.list), baseConfig())

			d := e.collect(t)

			assert.Equal(t, wantData(), d)
			golden(t, "collect.golden.json", d)
		})
	}
}

func TestCollectRequests(t *testing.T) {
	e := defaultEnv(t, "v2", baseConfig())

	e.collect(t)

	reqs := e.srv.requests()
	assert.Len(t, reqs, 1+1+3*4, "login, website list, then stats x2, pageviews and active per site")
	assert.Equal(t, 1, e.srv.count(http.MethodPost, "/api/auth/login"))
	assert.Equal(t, 1, e.auth.loginCount())
	for _, r := range reqs {
		if r.path == "/api/auth/login" {
			assert.Equal(t, "application/json", r.header.Get("Content-Type"))
			var body map[string]string
			require.NoError(t, json.Unmarshal([]byte(r.body), &body))
			assert.Equal(t, map[string]string{"username": testUser, "password": testPass}, body)
			continue
		}
		assert.Equal(t, "Bearer eyJhbGciOiJIUzI1NiJ9.e30.token1", r.header.Get("Authorization"), r.path)
		assert.Equal(t, "application/json", r.header.Get("Accept"), r.path)
	}

	var list seen
	var statsA, pageviewsA []seen
	for _, r := range reqs {
		switch r.path {
		case "/api/websites":
			list = r
		case "/api/websites/" + siteA + "/stats":
			statsA = append(statsA, r)
		case "/api/websites/" + siteA + "/pageviews":
			pageviewsA = append(pageviewsA, r)
		}
	}
	assert.Equal(t, "200", list.query.Get("pageSize"))

	nowMs, weekMs := testNow.UnixMilli(), (7 * 24 * time.Hour).Milliseconds()
	require.Len(t, statsA, 2)
	windows := map[string]string{}
	for _, r := range statsA {
		windows[r.query.Get("startAt")] = r.query.Get("endAt")
	}
	assert.Equal(t, map[string]string{
		fmt.Sprint(nowMs - weekMs):   fmt.Sprint(nowMs),
		fmt.Sprint(nowMs - 2*weekMs): fmt.Sprint(nowMs - weekMs),
	}, windows)

	require.Len(t, pageviewsA, 1)
	pv := pageviewsA[0].query
	assert.Equal(t, fmt.Sprint(time.Date(2026, 9, 24, 0, 0, 0, 0, paris).UnixMilli()), pv.Get("startAt"), "local midnight of the first bar")
	assert.Equal(t, fmt.Sprint(nowMs), pv.Get("endAt"))
	assert.Equal(t, "day", pv.Get("unit"))
	assert.Equal(t, "Europe/Paris", pv.Get("timezone"))
}

func TestLoginIsReusedAcrossCollects(t *testing.T) {
	e := defaultEnv(t, "v3", baseConfig())

	e.collect(t)
	e.clk.Advance(2 * time.Minute)
	e.collect(t)

	assert.Equal(t, 1, e.auth.loginCount())
}

func TestExpiredTokenIsRenewedOnce(t *testing.T) {
	a := newAuth()
	routes := routesFor(t, a, "v2", "websites_data.json")
	list := fixture(t, "websites_data.json")
	routes["GET /api/websites"] = a.guard(func(w http.ResponseWriter, _ *http.Request) {
		a.revokeAll()
		reply(w, http.StatusOK, list)
	})
	e := newEnv(t, a, routes, baseConfig())

	d := e.collect(t)

	assert.Len(t, d.Sites, 3)
	assert.Equal(t, 2, a.loginCount(), "the first login and one renewal shared by the concurrent sites")
}

func TestStaleCachedTokenIsRenewed(t *testing.T) {
	e := defaultEnv(t, "v2", baseConfig())
	e.c.token = "left-over-from-a-previous-run"

	d := e.collect(t)

	assert.Len(t, d.Sites, 3)
	assert.Equal(t, 1, e.auth.loginCount())
	assert.Equal(t, 1, e.srv.count(http.MethodPost, "/api/auth/login"))
}

func TestLoginFailure(t *testing.T) {
	cfg := baseConfig()
	cfg.Password = "wrong-password-value"
	e := defaultEnv(t, "v2", cfg)

	_, err := e.c.Collect(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "list websites: login: ")
	assert.Contains(t, err.Error(), "HTTP 401")
	assert.NotContains(t, err.Error(), "wrong-password-value")
	assert.NotContains(t, e.logs.String(), "wrong-password-value")
	assert.Equal(t, 1, e.auth.attemptCount())
	assert.Zero(t, e.srv.count(http.MethodGet, "/api/websites"))

	e.clk.Advance(2 * time.Minute)
	_, err = e.c.Collect(t.Context())
	require.Error(t, err)
	assert.Equal(t, 2, e.auth.attemptCount(), "a failed login is not cached")
}

func TestUnusableLoginResponses(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"no token", `{"user":{}}`, "login: response carries no token"},
		{"two-factor authentication", `{"requiresTwoFactor":true,"partialToken":"eyJhbGciOiJIUzI1NiJ9.e30.partial"}`, "two-factor authentication is enabled, use api_key instead"},
		{"not json", `<html>login page</html>`, "login: decode response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAuth()
			routes := routesFor(t, a, "v2", "websites_data.json")
			routes["POST /api/auth/login"] = func(w http.ResponseWriter, _ *http.Request) { reply(w, http.StatusOK, []byte(tt.body)) }
			e := newEnv(t, a, routes, baseConfig())

			_, err := e.c.Collect(t.Context())

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.NotContains(t, err.Error(), "partial")
			assert.NotContains(t, err.Error(), "login page")
		})
	}
}

func TestRejectedAfterRenewalIsAnError(t *testing.T) {
	a := newAuth()
	routes := routesFor(t, a, "v2", "websites_data.json")
	routes["GET /api/websites"] = func(w http.ResponseWriter, _ *http.Request) { reply(w, http.StatusUnauthorized, unauthorized) }
	e := newEnv(t, a, routes, baseConfig())

	_, err := e.c.Collect(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "list websites")
	assert.Contains(t, err.Error(), "HTTP 401")
	assert.Equal(t, 2, a.attemptCount(), "one login and a single renewal, no loop")
	assert.Equal(t, 2, e.srv.count(http.MethodGet, "/api/websites"))
}

func TestAPIKey(t *testing.T) {
	a := newAuth()
	a.apiKey = "umami_key_0123456789abcdef"
	cfg := config.Umami{Interval: time.Minute, APIKey: a.apiKey}
	e := newEnv(t, a, routesFor(t, a, "v3", "websites_data.json"), cfg)

	d := e.collect(t)

	assert.Equal(t, wantData(), d)
	assert.Zero(t, a.attemptCount())
	assert.Zero(t, e.srv.count(http.MethodPost, "/api/auth/login"))
	for _, r := range e.srv.requests() {
		assert.Equal(t, "Bearer "+a.apiKey, r.header.Get("Authorization"), r.path)
	}
}

func TestAPIKeyRejected(t *testing.T) {
	a := newAuth()
	a.apiKey = "the-real-key"
	cfg := baseConfig()
	cfg.APIKey = "a-revoked-key"
	e := newEnv(t, a, routesFor(t, a, "v3", "websites_data.json"), cfg)

	_, err := e.c.Collect(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 401")
	assert.NotContains(t, err.Error(), "a-revoked-key")
	assert.Zero(t, a.attemptCount(), "an API key never falls back to a login")
	assert.Equal(t, 1, e.srv.count(http.MethodGet, "/api/websites"))
}

func TestSitesFilterAndLabels(t *testing.T) {
	cfg := baseConfig()
	cfg.Sites = []config.UmamiSite{
		{ID: siteA, Label: "Maintenant"},
		{ID: siteC},
		{ID: "00000000-0000-0000-0000-000000000000", Label: "ghost"},
	}
	e := defaultEnv(t, "v2", cfg)

	d := e.collect(t)

	assert.Equal(t, []string{"Maintenant", "kolapsis.com"}, siteNames(d))
	assert.Equal(t, 812+96, d.Visits7)
	assert.Equal(t, 745+71, d.Prev7)
	assert.Equal(t, ptr(11.3), d.DeltaPct)
	assert.Equal(t, 2, d.Live)
	assert.Equal(t, dayPoints(days14, 118, 109, 41, 33, 127, 142, 132, 122, 137, 47, 38, 143, 149, 68), d.Bars14)
	assert.Zero(t, e.srv.count(http.MethodGet, "/api/websites/"+siteB+"/stats"))
	assert.Contains(t, e.logs.String(), "umami site not found")
}

func TestSitesMatchIDsCaseInsensitively(t *testing.T) {
	cfg := baseConfig()
	cfg.Sites = []config.UmamiSite{{ID: strings.ToUpper(siteB)}}
	e := defaultEnv(t, "v2", cfg)

	d := e.collect(t)

	assert.Equal(t, []string{"restoreproof.io"}, siteNames(d))
	assert.Equal(t, siteB, d.Sites[0].ID)
}

func TestSiteNameFallsBackToNameThenID(t *testing.T) {
	a := newAuth()
	routes := routesFor(t, a, "v2", "websites_data.json")
	routes["GET /api/websites"] = a.guard(func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, []byte(`{"data":[{"id":"x1","name":"Docs","domain":""},{"id":"x2"},{"id":"x3","name":"N","domain":"d.example"}]}`))
	})
	e := newEnv(t, a, routes, baseConfig())

	refs, err := e.c.sites(t.Context())

	require.NoError(t, err)
	assert.Equal(t, []siteRef{{"x1", "Docs"}, {"x2", "x2"}, {"x3", "d.example"}}, refs)
}

func TestConfiguredSitesThatMatchNothing(t *testing.T) {
	cfg := baseConfig()
	cfg.Sites = []config.UmamiSite{{ID: "nope", Label: "x"}}
	e := defaultEnv(t, "v2", cfg)

	_, err := e.c.Collect(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no website to report among the 3 listed")
}

func TestNoWebsiteAtAll(t *testing.T) {
	a := newAuth()
	routes := routesFor(t, a, "v2", "websites_data.json")
	routes["GET /api/websites"] = a.guard(func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, []byte(`{"data":[],"count":0,"page":1,"pageSize":200}`))
	})
	e := newEnv(t, a, routes, baseConfig())

	_, err := e.c.Collect(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no website to report among the 0 listed")
}

func TestWebsiteListFailure(t *testing.T) {
	a := newAuth()
	routes := routesFor(t, a, "v2", "websites_data.json")
	routes["GET /api/websites"] = a.guard(func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusInternalServerError, []byte(`{"error":"database down"}`))
	})
	e := newEnv(t, a, routes, baseConfig())

	_, err := e.c.Collect(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "list websites")
	assert.Contains(t, err.Error(), "HTTP 500")
	assert.Contains(t, err.Error(), "/api/websites?…")
	assert.NotContains(t, err.Error(), "pageSize")
}

func failing(a *auth, inner http.HandlerFunc, failID string, status int) http.HandlerFunc {
	return a.guard(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == failID {
			reply(w, status, []byte(`{"error":"boom"}`))
			return
		}
		inner(w, r)
	})
}

func TestOneFailingSiteIsSkipped(t *testing.T) {
	for _, endpoint := range []string{"stats", "pageviews"} {
		t.Run(endpoint, func(t *testing.T) {
			a := newAuth()
			routes := routesFor(t, a, "v2", "websites_data.json")
			pattern := "GET /api/websites/{id}/" + endpoint
			routes[pattern] = failing(a, routes[pattern], siteB, http.StatusInternalServerError)
			e := newEnv(t, a, routes, baseConfig())

			d := e.collect(t)

			assert.Equal(t, []string{"maintenant.dev", "kolapsis.com"}, siteNames(d))
			assert.Equal(t, 812+96, d.Visits7)
			assert.Equal(t, 745+71, d.Prev7)
			assert.Equal(t, 2, d.Live)
			assert.Contains(t, e.logs.String(), "umami site skipped")
			assert.Contains(t, e.logs.String(), "site=restoreproof.io")
		})
	}
}

func TestLiveVisitorsAreOptional(t *testing.T) {
	a := newAuth()
	routes := routesFor(t, a, "v3", "websites_data.json")
	routes["GET /api/websites/{id}/active"] = failing(a, routes["GET /api/websites/{id}/active"], siteA, http.StatusNotFound)
	e := newEnv(t, a, routes, baseConfig())

	d := e.collect(t)

	assert.Len(t, d.Sites, 3, "the site is kept")
	assert.Equal(t, 0, d.Sites[0].Live)
	assert.Equal(t, siteA, d.Sites[0].ID)
	assert.Equal(t, 1, d.Live)
	assert.Contains(t, e.logs.String(), "umami live visitors unavailable")
}

func TestAllSitesFailing(t *testing.T) {
	a := newAuth()
	routes := routesFor(t, a, "v2", "websites_data.json")
	routes["GET /api/websites/{id}/stats"] = a.guard(func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusBadGateway, []byte(`<html>bad gateway</html>`))
	})
	e := newEnv(t, a, routes, baseConfig())

	_, err := e.c.Collect(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "all 3 sites failed, first error: stats: ")
	assert.Contains(t, err.Error(), "HTTP 502")
	assert.Equal(t, 3, strings.Count(e.logs.String(), "umami site skipped"))
}

func TestCancellationIsReportedAsSuch(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := newAuth()
	routes := routesFor(t, a, "v2", "websites_data.json")
	routes["GET /api/websites/{id}/stats"] = func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		reply(w, http.StatusOK, []byte(`{"visits":1,"pageviews":1}`))
	}
	e := newEnv(t, a, routes, baseConfig())

	_, err := e.c.Collect(ctx)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), err.Error())
	assert.NotContains(t, err.Error(), "all 3 sites failed")
}

func TestSiteConcurrencyIsBounded(t *testing.T) {
	a := newAuth()
	var sites []map[string]string
	for i := 1; i <= 8; i++ {
		sites = append(sites, map[string]string{"id": fmt.Sprintf("site-%d", i), "name": fmt.Sprintf("Site %d", i), "domain": fmt.Sprintf("site%d.example", i)})
	}
	list, err := json.Marshal(map[string]any{"data": sites})
	require.NoError(t, err)
	routes := map[string]http.HandlerFunc{
		"POST /api/auth/login": a.login(t),
		"GET /api/websites":    a.guard(func(w http.ResponseWriter, _ *http.Request) { reply(w, http.StatusOK, list) }),
		"GET /api/websites/{id}/stats": a.guard(func(w http.ResponseWriter, _ *http.Request) {
			reply(w, http.StatusOK, []byte(`{"visits":10,"pageviews":20}`))
		}),
		"GET /api/websites/{id}/pageviews": a.guard(func(w http.ResponseWriter, _ *http.Request) {
			reply(w, http.StatusOK, []byte(`{"pageviews":[],"sessions":[]}`))
		}),
		"GET /api/websites/{id}/active": a.guard(func(w http.ResponseWriter, _ *http.Request) {
			reply(w, http.StatusOK, []byte(`{"visitors":1}`))
		}),
	}
	e := newEnv(t, a, routes, baseConfig())
	e.srv.delay = 15 * time.Millisecond

	d := e.collect(t)

	assert.Len(t, d.Sites, 8)
	assert.Equal(t, 80, d.Visits7)
	assert.Equal(t, 8, d.Live)
	assert.Equal(t, ptr(0.0), d.DeltaPct)
	assert.LessOrEqual(t, e.srv.concurrency(), siteWorkers)
	assert.Greater(t, e.srv.concurrency(), 1, "sites are collected concurrently")
	assert.Equal(t, "site1.example", d.Sites[0].Name, "ties are broken by name")
}

func TestNetworkErrorsAreRedacted(t *testing.T) {
	a := newAuth()
	a.apiKey = "umami_key_0123456789abcdef"
	cfg := config.Umami{Interval: time.Minute, APIKey: a.apiKey}
	e := newEnv(t, a, routesFor(t, a, "v2", "websites_data.json"), cfg)
	e.srv.Close()

	_, err := e.c.Collect(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "list websites: ")
	assert.Contains(t, err.Error(), "/api/websites?…")
	assert.NotContains(t, err.Error(), "pageSize")
	assert.NotContains(t, err.Error(), a.apiKey)
}

func TestLoginNetworkErrorKeepsThePasswordOut(t *testing.T) {
	e := defaultEnv(t, "v2", baseConfig())
	e.srv.Close()

	_, err := e.c.Collect(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "login: ")
	assert.NotContains(t, err.Error(), testPass)
	assert.NotContains(t, e.logs.String(), testPass)
}

func TestMetadata(t *testing.T) {
	c := New(config.Umami{Interval: 2 * time.Minute, BaseURL: "https://umami.example///"}, collector.Deps{})

	assert.Equal(t, "umami", c.Name())
	assert.Equal(t, 2*time.Minute, c.Interval())
	assert.Equal(t, "https://umami.example", c.base)
	var _ collector.Collector = c
	var _ collector.Summarizer = c
	_, hasTimeout := any(c).(collector.Timeouter)
	assert.False(t, hasTimeout, "the default scheduler timeout is enough")
}

func TestSummary(t *testing.T) {
	c := New(baseConfig(), collector.Deps{})
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"typical", Data{Visits7: 1148, DeltaPct: ptr(8.9), Live: 2, Sites: make([]Site, 3)}, "3 sites · 1 148 visites / 7 j (+8,9 %) · 2 en direct"},
		{"from the fixtures", wantData(), "3 sites · 1 148 visites / 7 j (+7,6 %) · 3 en direct"},
		{"decrease", Data{Visits7: 240, DeltaPct: ptr(-4.4), Sites: make([]Site, 2)}, "2 sites · 240 visites / 7 j (-4,4 %) · 0 en direct"},
		{"no previous week", Data{Visits7: 12, Live: 1, Sites: make([]Site, 1)}, "1 site · 12 visites / 7 j · 1 en direct"},
		{"singular", Data{Visits7: 1, DeltaPct: ptr(0.0), Sites: make([]Site, 1)}, "1 site · 1 visite / 7 j (+0,0 %) · 0 en direct"},
		{"large", Data{Visits7: 1234567, DeltaPct: ptr(120.5), Sites: make([]Site, 10)}, "10 sites · 1 234 567 visites / 7 j (+120,5 %) · 0 en direct"},
		{"foreign value", 42, ""},
		{"nil", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, c.Summary(tt.in))
		})
	}
}
