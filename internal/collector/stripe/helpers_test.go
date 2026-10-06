package stripe

import (
	"bytes"
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
	"github.com/kolapsis/pi-dashboard/internal/store"
)

const (
	keyMaintenant = "rk_test_fake_maintenant"
	keyRestore    = "rk_test_fake_restoreproof"
	keyAckify     = "rk_test_fake_ackify"

	lastSubOfPage1    = "sub_1Q9xFAKE0000000000000S03"
	lastChargeOfPage1 = "ch_3Q9xFAKE000000000000000C"
	invoiceOfLast     = "in_1Q9xFAKE0000000000000001"
)

var paris = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		panic(err)
	}
	return loc
}()

var testNow = time.Date(2026, 10, 7, 10, 0, 0, 0, paris)

func readFixture(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", name))
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func respond(t *testing.T, status int, name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body, err := readFixture(name)
		if err != nil {
			t.Errorf("fixture %s: %v", name, err)
			http.Error(w, "fixture missing", http.StatusInternalServerError)
			return
		}
		writeJSON(w, status, body)
	})
}

func signature(r *http.Request) string {
	q := r.URL.Query()
	parts := []string{r.URL.Path}
	if v := q.Get("status"); v != "" {
		parts = append(parts, "status="+v)
	}
	if q.Has("created[gte]") {
		parts = append(parts, "month")
	}
	if q.Get("limit") == "5" {
		parts = append(parts, "limit=5")
	}
	if v := q.Get("starting_after"); v != "" {
		parts = append(parts, "after="+v)
	}
	return strings.Join(parts, " ")
}

type routes map[string]string

func (rt routes) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := rt[signature(r)]
		if !ok {
			t.Errorf("unexpected request: %s", signature(r))
			writeJSON(w, http.StatusNotFound, []byte(`{"error":{"message":"no such route"}}`))
			return
		}
		body, err := readFixture(name)
		if err != nil {
			t.Errorf("fixture %s: %v", name, err)
			http.Error(w, "fixture missing", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, body)
	})
}

func maintenantRoutes() routes {
	return routes{
		"/v1/balance":                                             "balance.json",
		"/v1/subscriptions status=active":                         "subscriptions_active_1.json",
		"/v1/subscriptions status=past_due":                       "subscriptions_past_due.json",
		"/v1/charges month":                                       "charges_month_1.json",
		"/v1/invoices/" + invoiceOfLast:                           "invoice.json",
		"/v1/subscriptions status=active after=" + lastSubOfPage1: "subscriptions_active_2.json",
		"/v1/charges month after=" + lastChargeOfPage1:            "charges_month_2.json",
	}
}

func quietRoutes() routes {
	return routes{
		"/v1/balance":                       "balance_empty.json",
		"/v1/subscriptions status=active":   "list_empty.json",
		"/v1/subscriptions status=past_due": "list_empty.json",
		"/v1/charges month":                 "list_empty.json",
		"/v1/charges limit=5":               "list_empty.json",
	}
}

type request struct {
	key     string
	path    string
	version string
	query   url.Values
}

type fake struct {
	srv *httptest.Server

	mu       sync.Mutex
	handlers map[string]http.Handler
	reqs     []request
}

func newFake(t *testing.T) *fake {
	f := &fake{handlers: map[string]http.Handler{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		f.reqs = append(f.reqs, request{key: key, path: r.URL.Path, version: r.Header.Get("Stripe-Version"), query: r.URL.Query()})
		h := f.handlers[key]
		f.mu.Unlock()
		if h == nil {
			body, _ := readFixture("error_invalid_key.json")
			writeJSON(w, http.StatusUnauthorized, body)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) set(key string, h http.Handler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[key] = h
}

func (f *fake) requests(key string) []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []request
	for _, r := range f.reqs {
		if r.key == key {
			out = append(out, r)
		}
	}
	return out
}

func (f *fake) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reqs {
		if r.path == path {
			n++
		}
	}
	return n
}

type account struct {
	name, key string
	handler   http.Handler
}

type env struct {
	c    *Collector
	f    *fake
	clk  *clock.Fake
	hist *store.Mem
	logs *bytes.Buffer
}

func newEnv(t *testing.T, now time.Time, accounts ...account) *env {
	f := newFake(t)
	cfg := config.Stripe{Interval: 5 * time.Minute}
	for _, a := range accounts {
		cfg.Accounts = append(cfg.Accounts, config.StripeAccount{Name: a.name, Key: a.key})
		f.set(a.key, a.handler)
	}
	e := &env{f: f, clk: clock.NewFake(now), hist: store.NewMem(), logs: &bytes.Buffer{}}
	e.c = New(cfg, collector.Deps{
		HTTP:  f.srv.Client(),
		Clock: e.clk,
		Hist:  e.hist,
		Log:   slog.New(slog.NewTextHandler(e.logs, nil)),
		Loc:   paris,
	})
	e.c.baseURL = f.srv.URL
	return e
}

func (e *env) client() client {
	return client{http: e.f.srv.Client(), baseURL: e.f.srv.URL, key: e.c.cfg.Accounts[0].Key}
}

func (e *env) collect(t *testing.T) Data {
	t.Helper()
	got, err := e.c.Collect(t.Context())
	require.NoError(t, err)
	d, ok := got.(Data)
	require.True(t, ok, "Collect returned %T", got)
	return d
}
