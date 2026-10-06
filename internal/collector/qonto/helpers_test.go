package qonto

import (
	"bytes"
	"cmp"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
	slugSAS   = "kolapsis-1234"
	secretSAS = "FAKEsecretsas000000"
	slugEI    = "jean-dupont-1234"
	secretEI  = "FAKEsecretei0000000"

	idSASMain    = "0192e0a4-1b7c-7e3a-9d41-5c8f2a6b3e01"
	idSASSavings = "0192e0a4-1b7c-7e3a-9d41-5c8f2a6b3e02"
	idEIMain     = "0192e0b8-4d2e-7a15-8c63-9e0f1b7d4a01"
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

func respondRaw(status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, status, []byte(body))
	})
}

func signature(path string, q url.Values) string {
	if path == "/v2/organization" {
		return "organization"
	}
	kind := "recent"
	if q.Has("settled_at_from") {
		kind = "month"
	}
	return fmt.Sprintf("transactions %s %s %s", q.Get("bank_account_id"), kind, cmp.Or(q.Get("page"), "1"))
}

type routes map[string]string

func (rt routes) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sig := signature(r.URL.Path, r.URL.Query())
		name, ok := rt[sig]
		if !ok {
			t.Errorf("unexpected request: %s", sig)
			writeJSON(w, http.StatusNotFound, []byte(`{"errors":[{"code":"not_found"}]}`))
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

func sasRoutes() routes {
	return routes{
		"organization": "organization_sas.json",
		"transactions " + idSASMain + " recent 1":    "transactions_sas_main_recent.json",
		"transactions " + idSASMain + " month 1":     "transactions_sas_main_month_1.json",
		"transactions " + idSASMain + " month 2":     "transactions_sas_main_month_2.json",
		"transactions " + idSASSavings + " recent 1": "transactions_sas_savings_recent.json",
		"transactions " + idSASSavings + " month 1":  "transactions_sas_savings_month_1.json",
	}
}

func eiRoutes() routes {
	return routes{
		"organization":                           "organization_ei.json",
		"transactions " + idEIMain + " recent 1": "transactions_ei_main_recent.json",
		"transactions " + idEIMain + " month 1":  "transactions_ei_main_month_1.json",
	}
}

type request struct {
	auth  string
	path  string
	query url.Values
}

func (r request) signature() string { return signature(r.path, r.query) }

type fake struct {
	srv *httptest.Server

	mu       sync.Mutex
	handlers map[string]http.Handler
	reqs     []request
}

func newFake(t *testing.T) *fake {
	f := &fake{handlers: map[string]http.Handler{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		f.mu.Lock()
		f.reqs = append(f.reqs, request{auth: auth, path: r.URL.Path, query: r.URL.Query()})
		h := f.handlers[auth]
		f.mu.Unlock()
		if h == nil {
			body, _ := readFixture("error_unauthorized.json")
			writeJSON(w, http.StatusUnauthorized, body)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) set(auth string, h http.Handler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[auth] = h
}

func (f *fake) requests(auth string) []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []request
	for _, r := range f.reqs {
		if r.auth == auth {
			out = append(out, r)
		}
	}
	return out
}

type orgSetup struct {
	cfg     config.QontoOrg
	handler http.Handler
}

func (o orgSetup) auth() string { return o.cfg.Slug + ":" + o.cfg.Secret }

func sas(t *testing.T, warnBelow float64) orgSetup {
	return orgSetup{config.QontoOrg{Name: "kOlapsis SAS", Slug: slugSAS, Secret: secretSAS, WarnBelow: warnBelow}, sasRoutes().handler(t)}
}

func ei(t *testing.T) orgSetup {
	return orgSetup{config.QontoOrg{Name: "EI", Slug: slugEI, Secret: secretEI}, eiRoutes().handler(t)}
}

func (o orgSetup) with(h http.Handler) orgSetup {
	o.handler = h
	return o
}

type env struct {
	c    *Collector
	f    *fake
	clk  *clock.Fake
	hist *store.Mem
	logs *bytes.Buffer
}

func newEnv(t *testing.T, now time.Time, orgs ...orgSetup) *env {
	f := newFake(t)
	cfg := config.Qonto{Interval: 5 * time.Minute}
	for _, o := range orgs {
		cfg.Orgs = append(cfg.Orgs, o.cfg)
		f.set(o.auth(), o.handler)
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
	oc := e.c.cfg.Orgs[0]
	return client{http: e.f.srv.Client(), baseURL: e.f.srv.URL, slug: oc.Slug, secret: oc.Secret}
}

func (e *env) collect(t *testing.T) Data {
	t.Helper()
	got, err := e.c.Collect(t.Context())
	require.NoError(t, err)
	d, ok := got.(Data)
	require.True(t, ok, "Collect returned %T", got)
	return d
}
