package stripe

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/store"
)

var (
	_ collector.Collector  = (*Collector)(nil)
	_ collector.Summarizer = (*Collector)(nil)
	_ collector.Timeouter  = (*Collector)(nil)
)

func maintenant(t *testing.T) account {
	return account{"Maintenant", keyMaintenant, maintenantRoutes().handler(t)}
}

func denySubscriptions(t *testing.T, inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/subscriptions" {
			inner.ServeHTTP(w, r)
			return
		}
		body, err := readFixture("error_permission.json")
		if err != nil {
			t.Errorf("fixture: %v", err)
		}
		writeJSON(w, http.StatusForbidden, body)
	})
}

func noRequest(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s", r.URL.Path)
		writeJSON(w, http.StatusNotFound, []byte(`{"error":{"message":"unexpected"}}`))
	})
}

func dropSubscriptions(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/subscriptions" {
			inner.ServeHTTP(w, r)
			return
		}
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	})
}

func failOn(sig string, status int, body string, inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if signature(r) == sig {
			writeJSON(w, status, []byte(body))
			return
		}
		inner.ServeHTTP(w, r)
	})
}

func serverError() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, []byte(`{"error":{"message":"An unknown error occurred","type":"api_error"}}`))
	})
}

func TestMetadata(t *testing.T) {
	c := New(config.Stripe{Interval: 7 * time.Minute}, collector.Deps{})
	assert.Equal(t, "stripe", c.Name())
	assert.Equal(t, 7*time.Minute, c.Interval())
	assert.Equal(t, 45*time.Second, c.Timeout())
}

func TestCollect_Account(t *testing.T) {
	e := newEnv(t, testNow, maintenant(t))
	got := e.collect(t)

	assert.Equal(t, "eur", got.Currency)
	require.Len(t, got.Accounts, 1)
	assert.Equal(t, Account{
		Name:           "Maintenant",
		Livemode:       true,
		MRRCents:       19875,
		Subs:           5,
		MeteredSubs:    2,
		MonthNetCents:  15700,
		Payments:       3,
		AvailableCents: 31240,
		PendingCents:   14900,
		Last: &Payment{
			AmountCents: 2900,
			Currency:    "eur",
			Label:       "1 × Maintenant Pro (at €29.00 / month)",
			At:          time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC),
		},
	}, got.Accounts[0])
	assert.Equal(t, Totals{MRRCents: 19875, MonthNetCents: 15700, Payments: 3, AvailableCents: 31240, PendingCents: 14900}, got.Total)
}

func TestCollect_Requests(t *testing.T) {
	e := newEnv(t, testNow, maintenant(t))
	e.collect(t)

	reqs := e.f.requests(keyMaintenant)
	for _, r := range reqs {
		assert.Equal(t, apiVersion, r.version, r.path)
	}
	assert.Equal(t, 1, e.f.count("/v1/balance"))
	assert.Equal(t, 3, e.f.count("/v1/subscriptions"), "two active pages and one past_due page")
	assert.Equal(t, 2, e.f.count("/v1/charges"), "no fallback listing when the month has a payment")
	assert.Equal(t, 1, e.f.count("/v1/invoices/"+invoiceOfLast))

	var statuses []string
	for _, r := range reqs {
		if r.path == "/v1/subscriptions" {
			assert.Equal(t, "100", r.query.Get("limit"))
			statuses = append(statuses, r.query.Get("status")+":"+r.query.Get("starting_after"))
		}
		if r.path == "/v1/charges" {
			assert.Equal(t, "100", r.query.Get("limit"))
		}
	}
	assert.Equal(t, []string{"active:", "active:" + lastSubOfPage1, "past_due:"}, statuses)
}

func TestCollect_MonthBoundary(t *testing.T) {
	utc := func(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, time.UTC) }
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"00:30 on the 1st in Paris is already October", utc(2026, 9, 30, 22, 30), utc(2026, 9, 30, 22, 0)},
		{"mid October", utc(2026, 10, 15, 12, 0), utc(2026, 9, 30, 22, 0)},
		{"23:59 on the 31st, after the clocks went back", utc(2026, 10, 31, 22, 59), utc(2026, 9, 30, 22, 0)},
		{"00:30 on November 1st in Paris", utc(2026, 10, 31, 23, 30), utc(2026, 10, 31, 23, 0)},
		{"00:30 on April 1st, summer time", utc(2026, 3, 31, 22, 30), utc(2026, 3, 31, 22, 0)},
		{"23:30 on March 31st, summer time", utc(2026, 3, 31, 21, 30), utc(2026, 2, 28, 23, 0)},
		{"00:30 on January 1st", utc(2026, 12, 31, 23, 30), utc(2026, 12, 31, 23, 0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.now, account{"RestoreProof", keyRestore, quietRoutes().handler(t)})
			e.collect(t)
			var got []string
			for _, r := range e.f.requests(keyRestore) {
				if r.query.Has("created[gte]") {
					got = append(got, r.query.Get("created[gte]"))
				}
			}
			assert.Equal(t, []string{strconv.FormatInt(tt.want.Unix(), 10)}, got)
		})
	}
}

func TestCollect_LastPaymentFallsBackToRecentCharges(t *testing.T) {
	routes := quietRoutes()
	routes["/v1/charges limit=5"] = "charges_recent.json"
	e := newEnv(t, testNow, account{"RestoreProof", keyRestore, routes.handler(t)})
	got := e.collect(t).Accounts[0]

	var listings []bool
	for _, r := range e.f.requests(keyRestore) {
		if r.path == "/v1/charges" {
			listings = append(listings, r.query.Has("created[gte]"))
		}
	}
	assert.Equal(t, []bool{true, false}, listings, "month listing, then the unfiltered one")
	assert.Zero(t, got.Payments)
	assert.Zero(t, got.MonthNetCents)
	assert.Equal(t, &Payment{
		AmountCents: 12900,
		Currency:    "eur",
		Label:       "Licence Personal",
		At:          time.Unix(1790520000, 0).UTC(),
	}, got.Last)
}

func TestCollect_NoPaymentAtAll(t *testing.T) {
	e := newEnv(t, testNow, account{"Ackify", keyAckify, quietRoutes().handler(t)})
	got := e.collect(t).Accounts[0]
	assert.Nil(t, got.Last)
	assert.Equal(t, Account{Name: "Ackify", Livemode: true}, got)
}

func TestCollect_PartialFailureKeepsLastGoodAccountStale(t *testing.T) {
	e := newEnv(t, testNow,
		maintenant(t),
		account{"RestoreProof", keyRestore, denySubscriptions(t, quietRoutes().handler(t))},
	)

	first := e.collect(t)
	require.Len(t, first.Accounts, 2)
	fresh := first.Accounts[0]
	assert.False(t, fresh.Stale)
	assert.Empty(t, fresh.Error)
	denied := first.Accounts[1]
	assert.Equal(t, "RestoreProof", denied.Name)
	assert.True(t, denied.Stale)
	assert.Equal(t, "subscriptions: HTTP 403: missing permission rak_subscription_read", denied.Error)
	assert.Zero(t, denied.MRRCents)
	assert.Equal(t, fresh.MRRCents, first.Total.MRRCents)
	assert.Nil(t, first.Total.MRRDelta30Cents)
	assert.Empty(t, first.Series30, "an incomplete total is not recorded")
	assert.NotNil(t, first.Series30)

	e.f.set(keyMaintenant, serverError())
	second := e.collect(t)
	cached := second.Accounts[0]
	assert.True(t, cached.Stale)
	assert.Equal(t, "balance: HTTP 500: An unknown error occurred", cached.Error)
	cached.Stale, cached.Error = false, ""
	assert.Equal(t, fresh, cached, "values of the last good fetch are kept")
	assert.Equal(t, first.Total, second.Total)

	e.f.set(keyMaintenant, maintenantRoutes().handler(t))
	e.f.set(keyRestore, quietRoutes().handler(t))
	third := e.collect(t)
	for _, a := range third.Accounts {
		assert.False(t, a.Stale, a.Name)
		assert.Empty(t, a.Error, a.Name)
	}
	assert.Len(t, third.Series30, 1)

	logs := e.logs.String()
	assert.Contains(t, logs, "stripe account failed")
	assert.NotContains(t, logs, "rk_live_")
	assert.NotContains(t, first.Accounts[1].Error, "rk_live_")
}

func TestCollect_EveryAccountFailedAndNothingCached(t *testing.T) {
	e := newEnv(t, testNow,
		account{"Maintenant", keyMaintenant, denySubscriptions(t, maintenantRoutes().handler(t))},
		account{"RestoreProof", keyRestore, denySubscriptions(t, quietRoutes().handler(t))},
	)
	got, err := e.c.Collect(t.Context())
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "Maintenant: subscriptions: HTTP 403: missing permission rak_subscription_read")
	assert.Contains(t, err.Error(), "RestoreProof: subscriptions: HTTP 403")
	for _, secret := range []string{"rk_live_", keyMaintenant, keyRestore} {
		assert.NotContains(t, err.Error(), secret)
		assert.NotContains(t, e.logs.String(), secret)
	}
}

func TestCollect_EveryAccountFailedButCached(t *testing.T) {
	e := newEnv(t, testNow, maintenant(t))
	good := e.collect(t).Accounts[0]

	e.f.set(keyMaintenant, serverError())
	got := e.collect(t)
	require.Len(t, got.Accounts, 1)
	assert.True(t, got.Accounts[0].Stale)
	assert.Equal(t, good.MRRCents, got.Total.MRRCents)
}

func TestCollect_EndpointFailures(t *testing.T) {
	const rateLimited = `{"error":{"message":"Request rate limit exceeded"}}`
	tests := []struct {
		name   string
		base   routes
		sig    string
		status int
		body   string
		want   string
	}{
		{"balance behind a failing proxy", maintenantRoutes(), "/v1/balance", http.StatusBadGateway, "<html>Bad Gateway</html>", "balance: HTTP 502"},
		{"balance that is not JSON", maintenantRoutes(), "/v1/balance", http.StatusOK, "<html>", "balance: decode: "},
		{"second page of subscriptions", maintenantRoutes(), "/v1/subscriptions status=active after=" + lastSubOfPage1, http.StatusTooManyRequests, rateLimited, "subscriptions: HTTP 429: Request rate limit exceeded"},
		{"past due subscriptions", maintenantRoutes(), "/v1/subscriptions status=past_due", http.StatusTooManyRequests, rateLimited, "subscriptions: HTTP 429: Request rate limit exceeded"},
		{"month listing", maintenantRoutes(), "/v1/charges month", http.StatusTooManyRequests, rateLimited, "charges: HTTP 429: Request rate limit exceeded"},
		{"second page of the month listing", maintenantRoutes(), "/v1/charges month after=" + lastChargeOfPage1, http.StatusTooManyRequests, rateLimited, "charges: HTTP 429: Request rate limit exceeded"},
		{"unfiltered listing", quietRoutes(), "/v1/charges limit=5", http.StatusTooManyRequests, rateLimited, "charges: HTTP 429: Request rate limit exceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, testNow, account{"Maintenant", keyMaintenant, failOn(tt.sig, tt.status, tt.body, tt.base.handler(t))})
			_, err := e.c.Collect(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "Maintenant: "+tt.want)
		})
	}
}

func TestCollect_InvalidKey(t *testing.T) {
	e := newEnv(t, testNow, account{"Maintenant", keyMaintenant, nil})
	_, err := e.c.Collect(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "balance: HTTP 401: Invalid API Key provided: <key>")
	assert.NotContains(t, err.Error(), "rk_live_")
}

func TestCollect_TransportErrorLeavesNoQueryString(t *testing.T) {
	e := newEnv(t, testNow, account{"Maintenant", keyMaintenant, dropSubscriptions(maintenantRoutes().handler(t))})
	_, err := e.c.Collect(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Maintenant: subscriptions: EOF")
	assert.NotContains(t, err.Error(), "?")
	assert.NotContains(t, err.Error(), "status=")
	assert.NotContains(t, e.logs.String(), "status=")
}

func TestCollect_History(t *testing.T) {
	t.Run("without past data", func(t *testing.T) {
		e := newEnv(t, testNow, maintenant(t))
		got := e.collect(t)
		assert.Nil(t, got.Total.MRRDelta30Cents)
		assert.Equal(t, []store.Point{{T: testNow.UTC().Truncate(store.Bucket), V: 19875}}, got.Series30)
	})
	t.Run("with a point 30 days back", func(t *testing.T) {
		e := newEnv(t, testNow, maintenant(t))
		require.NoError(t, e.hist.Put(t.Context(), "stripe", "mrr.total", testNow.Add(-30*24*time.Hour), 15000))
		got := e.collect(t)
		require.NotNil(t, got.Total.MRRDelta30Cents)
		assert.Equal(t, int64(4875), *got.Total.MRRDelta30Cents)
		require.Len(t, got.Series30, 2)
		assert.Equal(t, 15000.0, got.Series30[0].V)
		assert.Equal(t, 19875.0, got.Series30[1].V)
	})
}

func TestLabel(t *testing.T) {
	const withInvoice = invoiceOfLast
	tests := []struct {
		name    string
		charge  charge
		invoice http.Handler
		want    string
		calls   int
	}{
		{
			name:   "description wins",
			charge: charge{ID: "ch_1", Description: "Licence Personal", Invoice: withInvoice, StatementDescriptor: "KOLAPSIS"},
			want:   "Licence Personal",
		},
		{
			name:    "blank description falls through to the invoice line",
			charge:  charge{ID: "ch_2", Description: "  ", Invoice: withInvoice},
			invoice: respond(t, http.StatusOK, "invoice.json"),
			want:    "1 × Maintenant Pro (at €29.00 / month)",
			calls:   1,
		},
		{
			name:    "invoice without permission falls through to the statement descriptor",
			charge:  charge{ID: "ch_3", Invoice: withInvoice, StatementDescriptor: "MAINTENANT", CalculatedStatementDescriptor: "KOLAPSIS"},
			invoice: respond(t, http.StatusForbidden, "error_permission.json"),
			want:    "MAINTENANT",
			calls:   1,
		},
		{
			name:    "invoice without lines falls through to the calculated descriptor",
			charge:  charge{ID: "ch_4", Invoice: withInvoice, CalculatedStatementDescriptor: "KOLAPSIS"},
			invoice: respond(t, http.StatusOK, "list_empty.json"),
			want:    "KOLAPSIS",
			calls:   1,
		},
		{
			name:   "no invoice, calculated descriptor",
			charge: charge{ID: "ch_5", CalculatedStatementDescriptor: "KOLAPSIS"},
			want:   "KOLAPSIS",
		},
		{
			name:   "nothing at all",
			charge: charge{ID: "ch_6"},
			want:   "Paiement",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			invoice := tt.invoice
			if invoice == nil {
				invoice = noRequest(t)
			}
			e := newEnv(t, testNow, account{"Maintenant", keyMaintenant, invoice})
			got := e.c.label(t.Context(), e.client(), "Maintenant", tt.charge)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.calls, e.f.count("/v1/invoices/"+withInvoice))
		})
	}
}

func TestLabelCache(t *testing.T) {
	ch := charge{ID: "ch_9", Invoice: invoiceOfLast, CalculatedStatementDescriptor: "KOLAPSIS"}
	path := "/v1/invoices/" + invoiceOfLast

	t.Run("a resolved invoice is fetched once", func(t *testing.T) {
		e := newEnv(t, testNow, account{"Maintenant", keyMaintenant, respond(t, http.StatusOK, "invoice.json")})
		for range 3 {
			assert.Equal(t, "1 × Maintenant Pro (at €29.00 / month)", e.c.label(t.Context(), e.client(), "Maintenant", ch))
		}
		assert.Equal(t, 1, e.f.count(path))
	})
	t.Run("a missing permission is not retried for the same charge", func(t *testing.T) {
		e := newEnv(t, testNow, account{"Maintenant", keyMaintenant, respond(t, http.StatusForbidden, "error_permission.json")})
		for range 3 {
			assert.Equal(t, "KOLAPSIS", e.c.label(t.Context(), e.client(), "Maintenant", ch))
		}
		assert.Equal(t, 1, e.f.count(path))
	})
	t.Run("a transient failure is retried on the next cycle", func(t *testing.T) {
		e := newEnv(t, testNow, account{"Maintenant", keyMaintenant, serverError()})
		assert.Equal(t, "KOLAPSIS", e.c.label(t.Context(), e.client(), "Maintenant", ch))
		e.f.set(keyMaintenant, respond(t, http.StatusOK, "invoice.json"))
		assert.Equal(t, "1 × Maintenant Pro (at €29.00 / month)", e.c.label(t.Context(), e.client(), "Maintenant", ch))
		assert.Equal(t, 2, e.f.count(path))
	})
}

func TestListAllGuards(t *testing.T) {
	type obj struct {
		ID string `json:"id"`
	}
	id := func(o obj) string { return o.ID }

	t.Run("has_more on an empty page", func(t *testing.T) {
		e := newEnv(t, testNow, account{"x", keyMaintenant, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, []byte(`{"data":[],"has_more":true}`))
		})})
		err := listAll(t.Context(), e.client(), "/v1/charges", url.Values{}, id, func(obj) {})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "has_more set on an empty page")
	})
	t.Run("endless listing", func(t *testing.T) {
		var pages atomic.Int64
		e := newEnv(t, testNow, account{"x", keyMaintenant, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, []byte(`{"data":[{"id":"obj_`+strconv.FormatInt(pages.Add(1), 10)+`"}],"has_more":true}`))
		})})
		seen := 0
		err := listAll(t.Context(), e.client(), "/v1/charges", url.Values{}, id, func(obj) { seen++ })
		require.Error(t, err)
		assert.Contains(t, err.Error(), "more than 50 pages")
		assert.Equal(t, maxPages, seen)
	})
}

func TestSummary(t *testing.T) {
	c := New(config.Stripe{}, collector.Deps{})
	totals := Totals{MRRCents: 20300, MonthNetCents: 44700, Payments: 3}

	tests := []struct {
		name string
		data any
		want string
	}{
		{"nominal", Data{Total: totals, Accounts: make([]Account, 3)}, "MRR 203 € · mois 447 € (3 paiements) · 3 comptes"},
		{"singular", Data{Total: Totals{MRRCents: 2900, MonthNetCents: 2900, Payments: 1}, Accounts: make([]Account, 1)}, "MRR 29 € · mois 29 € (1 paiement) · 1 compte"},
		{"thousands and rounding", Data{Total: Totals{MRRCents: 123456, MonthNetCents: 99}, Accounts: make([]Account, 2)}, "MRR 1 235 € · mois 1 € (0 paiement) · 2 comptes"},
		{"failing account", Data{Total: totals, Accounts: []Account{{}, {Stale: true}, {Stale: true}}}, "MRR 203 € · mois 447 € (3 paiements) · 3 comptes (2 en erreur)"},
		{"foreign type", "nope", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, c.Summary(tt.data))
		})
	}
}

func TestEuros(t *testing.T) {
	tests := []struct {
		cents int64
		want  string
	}{
		{0, "0"},
		{49, "0"},
		{50, "1"},
		{100, "1"},
		{99999, "1 000"},
		{123456, "1 235"},
		{100000000, "1 000 000"},
		{-49, "0"},
		{-50, "−1"},
		{-123456, "−1 235"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, euros(tt.cents), strconv.FormatInt(tt.cents, 10))
	}
}

func TestAPIMessage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"permission", `{"error":{"message":"The provided key 'rk_live_***AbCd12' does not have the required permissions. Having the 'rak_charge_read' permission would allow this request to continue."}}`, "missing permission rak_charge_read"},
		{"forbidden without permission name", `{"error":{"message":"Your account cannot make live charges. Key rk_live_***AbCd12 was used."}}`, "Your account cannot make live charges. Key <key> was used."},
		{"invalid key", `{"error":{"message":"Invalid API Key provided: rk_live_***AbCd12"}}`, "Invalid API Key provided: <key>"},
		{"secret key", `{"error":{"message":"Invalid API Key provided: sk_test_***AbCd12"}}`, "Invalid API Key provided: <key>"},
		{"rate limit", `{"error":{"message":"Request rate limit exceeded"}}`, "Request rate limit exceeded"},
		{"not json", `<html>Bad gateway</html>`, ""},
		{"empty", ``, ""},
		{"long", `{"error":{"message":"` + strings.Repeat("x", 300) + `"}}`, strings.Repeat("x", maxErrorLen) + "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, apiMessage([]byte(tt.body)))
		})
	}
}
