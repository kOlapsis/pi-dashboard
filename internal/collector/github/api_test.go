package github

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/httpx"
)

func TestRateLimit(t *testing.T) {
	reset := time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC)
	tests := []struct {
		name    string
		status  int
		reply   func(t *testing.T, w http.ResponseWriter)
		wantRL  bool
		wantMsg string
	}{
		{
			name: "primary limit on 403", status: http.StatusForbidden, wantRL: true, wantMsg: "github: rate limit exceeded, resets at 14:30",
			reply: func(t *testing.T, w http.ResponseWriter) { rateLimited(t, w, http.StatusForbidden, reset) },
		},
		{
			name: "primary limit on 429", status: http.StatusTooManyRequests, wantRL: true, wantMsg: "resets at 14:30",
			reply: func(t *testing.T, w http.ResponseWriter) { rateLimited(t, w, http.StatusTooManyRequests, reset) },
		},
		{
			name: "secondary limit", status: http.StatusForbidden, wantRL: true, wantMsg: "github: rate limited, retry in 1m0s",
			reply: func(t *testing.T, w http.ResponseWriter) {
				reply(w, http.StatusForbidden, fixture(t, "rate_limited.json"), "Retry-After", "60", "X-RateLimit-Remaining", "412")
			},
		},
		{
			name: "limit without reset header", status: http.StatusForbidden, wantRL: true, wantMsg: "github: rate limited",
			reply: func(t *testing.T, w http.ResponseWriter) {
				reply(w, http.StatusForbidden, fixture(t, "rate_limited.json"), "X-RateLimit-Remaining", "0")
			},
		},
		{
			name: "plain 403 is not a rate limit", status: http.StatusForbidden,
			reply: func(t *testing.T, w http.ResponseWriter) {
				reply(w, http.StatusForbidden, fixture(t, "forbidden_traffic.json"), "X-RateLimit-Remaining", "4990")
			},
		},
		{
			name: "remaining quota on 429 is not a rate limit", status: http.StatusTooManyRequests,
			reply: func(t *testing.T, w http.ResponseWriter) {
				reply(w, http.StatusTooManyRequests, []byte(`{"message":"slow down"}`), "X-RateLimit-Remaining", "10")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routes := defaultRoutes(t)
			routes["GET /orgs/{org}/repos"] = func(w http.ResponseWriter, _ *http.Request) { tt.reply(t, w) }
			e := newEnv(t, routes, baseConfig())

			_, err := e.c.Collect(t.Context())

			require.Error(t, err)
			var rl *rateLimitError
			if tt.wantRL {
				require.True(t, errors.As(err, &rl), err.Error())
				assert.Contains(t, err.Error(), tt.wantMsg)
				return
			}
			assert.False(t, errors.As(err, &rl))
			var se *httpx.StatusError
			require.True(t, errors.As(err, &se))
			assert.Equal(t, tt.status, se.Status)
		})
	}
}

func TestRateLimitResetIsShownInLocalTime(t *testing.T) {
	reset := time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC)
	e := newEnv(t, map[string]http.HandlerFunc{
		"GET /orgs/{org}/repos": func(w http.ResponseWriter, _ *http.Request) { rateLimited(t, w, http.StatusForbidden, reset) },
	}, baseConfig())

	_, err := e.c.Collect(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "resets at 10:00", "Europe/Paris is UTC+1 in January")
}

func TestErrorsNeverLeakQueriesOrTokens(t *testing.T) {
	e := newEnv(t, defaultRoutes(t), baseConfig())
	e.srv.Close()

	_, err := e.c.Collect(t.Context())

	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "/orgs/kOlapsis/repos?…")
	assert.NotContains(t, msg, "per_page")
	assert.NotContains(t, msg, "sort=pushed")
	assert.NotContains(t, msg, testToken)
}

func TestHTTPFailureCarriesContext(t *testing.T) {
	routes := defaultRoutes(t)
	routes["GET /repos/{owner}/{repo}/pulls"] = func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusInternalServerError, []byte(`{"message":"Server Error"}`))
	}
	e := newEnv(t, routes, baseConfig())

	_, err := e.c.Collect(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kOlapsis/maintenant: open pull requests: ")
	assert.Contains(t, err.Error(), "HTTP 500")
	assert.Contains(t, err.Error(), "/pulls?…")
	assert.NotContains(t, err.Error(), "state=open")
}

func TestUnexpectedPayloadIsAnError(t *testing.T) {
	routes := defaultRoutes(t)
	routes["GET /orgs/{org}/repos"] = func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, []byte(`<html>maintenance</html>`))
	}
	e := newEnv(t, routes, baseConfig())

	_, err := e.c.Collect(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "list kOlapsis repositories")
	assert.Contains(t, err.Error(), "decode")
	assert.NotContains(t, err.Error(), "maintenance")
}

func TestPaginationNeverLeavesTheAPIHost(t *testing.T) {
	routes := defaultRoutes(t)
	routes["GET /orgs/{org}/repos"] = func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, []byte("[]"), "Link", `<https://evil.example/orgs/kOlapsis/repos?page=2>; rel="next"`)
	}
	e := newEnv(t, routes, baseConfig())

	_, err := e.c.Collect(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pagination link leaves")
	assert.NotContains(t, err.Error(), "evil.example")
	assert.Len(t, e.srv.requests(), 1)
}

func TestPaginationHasALimit(t *testing.T) {
	routes := defaultRoutes(t)
	routes["GET /orgs/{org}/repos"] = func(w http.ResponseWriter, r *http.Request) {
		next := fmt.Sprintf("http://%s/orgs/kOlapsis/repos?page=%d", r.Host, atoiOr(r.URL.Query().Get("page"), 1)+1)
		reply(w, http.StatusOK, []byte("[]"), "Link", `<`+next+`>; rel="next"`)
	}
	e := newEnv(t, routes, baseConfig())

	_, err := e.c.Collect(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "more than 1000 repositories")
	assert.Len(t, e.srv.requests(), maxRepoPages)
}

func TestTokenExpiryIsOptional(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   *time.Time
	}{
		{"absent", "", nil},
		{"garbage", "someday", nil},
		{"utc", "2027-01-01 12:00:00 UTC", ptr(time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC))},
		{"numeric offset", "2027-01-01 12:00:00 +0200", ptr(time.Date(2027, 1, 1, 10, 0, 0, 0, time.UTC))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, defaultRoutes(t), baseConfig())
			e.srv.expiry = tt.header
			e.put(t, keyTotal, testNow.Add(-week), 1)

			d := e.collect(t)

			if tt.want == nil {
				assert.Nil(t, d.TokenExpiresAt)
				return
			}
			require.NotNil(t, d.TokenExpiresAt)
			assert.True(t, tt.want.Equal(*d.TokenExpiresAt), d.TokenExpiresAt.String())
		})
	}
}

func TestTokenExpirySurvivesAResponseWithoutTheHeader(t *testing.T) {
	e := newEnv(t, defaultRoutes(t), baseConfig())
	e.srv.expiry = "2027-01-01 12:00:00 UTC"
	e.put(t, keyTotal, testNow.Add(-week), 1)
	e.collect(t)

	e.srv.expiry = ""
	d := e.collect(t)

	require.NotNil(t, d.TokenExpiresAt)
	assert.Equal(t, 2027, d.TokenExpiresAt.Year())
}

func TestLinks(t *testing.T) {
	const next = "https://api.github.com/organizations/187254031/repos?per_page=100&page=2"
	const last = "https://api.github.com/organizations/187254031/repos?per_page=100&page=7"
	tests := []struct {
		name   string
		values []string
		want   map[string]string
	}{
		{"none", nil, map[string]string{}},
		{"next and last", []string{`<` + next + `>; rel="next", <` + last + `>; rel="last"`}, map[string]string{"next": next, "last": last}},
		{"several header lines", []string{`<` + next + `>; rel="next"`, `<` + last + `>; rel="last"`}, map[string]string{"next": next, "last": last}},
		{"unquoted rel", []string{`<` + next + `>; rel=next`}, map[string]string{"next": next}},
		{"extra parameter", []string{`<` + next + `>; rel="next"; title="Page 2"`}, map[string]string{"next": next}},
		{"no space after the semicolon", []string{`<` + next + `>;rel="next"`}, map[string]string{"next": next}},
		{"not a link", []string{"nonsense"}, map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for _, v := range tt.values {
				h.Add("Link", v)
			}
			assert.Equal(t, tt.want, links(h))
		})
	}
}

func TestLastPage(t *testing.T) {
	tests := []struct {
		name string
		link string
		want int
	}{
		{"count", `<https://api.github.com/repositories/1/pulls?state=open&per_page=1&page=7>; rel="next", <https://api.github.com/repositories/1/pulls?state=open&per_page=1&page=7>; rel="last"`, 7},
		{"page not last in the query", `<https://api.github.com/repositories/1/pulls?page=12&state=open&per_page=1>; rel="last"`, 12},
		{"no last relation", `<https://api.github.com/repositories/1/pulls?page=2>; rel="next"`, 0},
		{"no page parameter", `<https://api.github.com/repositories/1/pulls>; rel="last"`, 0},
		{"page is not a number", `<https://api.github.com/repositories/1/pulls?page=x>; rel="last"`, 0},
		{"negative page", `<https://api.github.com/repositories/1/pulls?page=-3>; rel="last"`, 0},
		{"no header", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.link != "" {
				h.Set("Link", tt.link)
			}
			assert.Equal(t, tt.want, lastPage(h))
		})
	}
}

func TestParseExpiry(t *testing.T) {
	tests := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"2027-01-01 12:00:00 UTC", time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC), true},
		{" 2027-01-01 12:00:00 UTC\n", time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC), true},
		{"2027-01-01 12:00:00 -0500", time.Date(2027, 1, 1, 17, 0, 0, 0, time.UTC), true},
		{"2027-01-01T12:00:00Z", time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC), true},
		{"", time.Time{}, false},
		{"2027-01-01", time.Time{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := parseExpiry(tt.in)
			require.Equal(t, tt.ok, ok)
			if ok {
				assert.True(t, tt.want.Equal(got), got.String())
			}
		})
	}
}

func TestRedactErr(t *testing.T) {
	raw := &url.Error{Op: "Get", URL: "https://api.github.com/orgs/x/repos?per_page=100&access_token=s3cret", Err: errors.New("connection refused")}

	got := redactErr(fmt.Errorf("wrapped: %w", raw))

	assert.Equal(t, "Get https://api.github.com/orgs/x/repos?…: connection refused", got.Error())
	assert.NotContains(t, got.Error(), "s3cret")
	assert.NoError(t, redactErr(nil))
	plain := errors.New("plain")
	assert.Equal(t, plain, redactErr(plain))
}

func TestGroupDigits(t *testing.T) {
	for in, want := range map[int]string{0: "0", 7: "7", 999: "999", 1000: "1 000", 1148: "1 148", 1234567: "1 234 567", -1500: "-1 500", -12: "-12"} {
		assert.Equal(t, want, groupDigits(in), fmt.Sprint(in))
	}
}
