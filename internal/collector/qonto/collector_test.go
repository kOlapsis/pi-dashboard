package qonto

import (
	"context"
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

func utc(m time.Month, d, h, min, s int) time.Time {
	return time.Date(2026, m, d, h, min, s, 0, time.UTC)
}

func dropTransactions(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/transactions" {
			inner.ServeHTTP(w, r)
			return
		}
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	})
}

func unauthorized(t *testing.T) http.Handler {
	return respond(t, http.StatusUnauthorized, "error_unauthorized.json")
}

func failOn(sig string, status int, body string, inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if signature(r.URL.Path, r.URL.Query()) == sig {
			writeJSON(w, status, []byte(body))
			return
		}
		inner.ServeHTTP(w, r)
	})
}

func serverError() http.Handler {
	return respondRaw(http.StatusInternalServerError, `{"errors":[{"code":"internal_server_error","detail":"boom"}]}`)
}

var wantSAS = Org{
	Name:       "kOlapsis SAS",
	TotalCents: 1398032,
	Accounts: []Account{
		{Name: "Compte principal", BalanceCents: 1248032, AuthorizedCents: 1239032, Main: true},
		{Name: "Compte épargne", BalanceCents: 150000, AuthorizedCents: 150000},
	},
	Month: Month{InCents: 145950, OutCents: 105598},
	Recent: []Transaction{
		{Label: "Stripe Payments UK Ltd", Counterparty: "Stripe", AmountCents: 44700, Side: "credit", Type: "income", At: utc(10, 6, 7, 12, 41)},
		{Label: "OVH SAS", Counterparty: "OVH", AmountCents: 2398, Side: "debit", Type: "direct_debit", At: utc(10, 5, 4, 30, 10)},
		{Label: "Intérêts créditeurs", Counterparty: "Qonto", AmountCents: 1250, Side: "credit", Type: "income", At: utc(10, 4, 12, 0, 0)},
		{Label: "Cabinet expert-comptable", Counterparty: "Cabinet expert-comptable", AmountCents: 42000, Side: "debit", Type: "transfer", At: utc(10, 3, 9, 45, 0)},
		{Label: "URSSAF", Counterparty: "URSSAF", AmountCents: 61200, Side: "debit", Type: "direct_debit", At: utc(10, 2, 6, 0, 0)},
		{Label: "VIR ASSOCIE APPORT CCA", Counterparty: "Associé", AmountCents: 100000, Side: "credit", Type: "transfer", At: utc(10, 1, 10, 15, 30)},
		{Label: "Notion Labs Inc", Counterparty: "Notion", AmountCents: 4900, Side: "debit", Type: "card", At: utc(9, 30, 14, 20, 0)},
		{Label: "Frais de tenue de compte", Counterparty: "Qonto", AmountCents: 1200, Side: "debit", Type: "qonto_fee", At: utc(9, 29, 8, 0, 0)},
	},
}

var wantEI = Org{
	Name:       "EI",
	TotalCents: 321010,
	Accounts:   []Account{{Name: "Compte courant", BalanceCents: 321010, AuthorizedCents: 321010, Main: true}},
	Month:      Month{InCents: 225000, OutCents: 61100},
	Recent: []Transaction{
		{Label: "Notion Labs Inc", Counterparty: "Notion", AmountCents: 4900, Side: "debit", Type: "card", At: utc(10, 3, 15, 30, 0)},
		{Label: "VIR SEPA RECU", Counterparty: "Client", AmountCents: 225000, Side: "credit", Type: "income", At: utc(10, 3, 8, 0, 0)},
		{Label: "URSSAF", Counterparty: "URSSAF", AmountCents: 56200, Side: "debit", Type: "direct_debit", At: utc(10, 2, 5, 0, 0)},
	},
}

func TestMetadata(t *testing.T) {
	c := New(config.Qonto{Interval: 7 * time.Minute}, collector.Deps{})
	assert.Equal(t, "qonto", c.Name())
	assert.Equal(t, 7*time.Minute, c.Interval())
	assert.Equal(t, 45*time.Second, c.Timeout())
}

func TestCollect_Orgs(t *testing.T) {
	e := newEnv(t, testNow, sas(t, 5000), ei(t))
	got := e.collect(t)

	assert.Equal(t, int64(1719042), got.TotalCents)
	require.Len(t, got.Orgs, 2)
	assert.Equal(t, wantSAS, got.Orgs[0])
	assert.Equal(t, wantEI, got.Orgs[1])
}

func TestCollect_Requests(t *testing.T) {
	e := newEnv(t, testNow, sas(t, 0))
	e.collect(t)

	reqs := e.f.requests(slugSAS + ":" + secretSAS)
	var got []string
	for _, r := range reqs {
		got = append(got, r.signature())
		if r.path != "/v2/transactions" {
			continue
		}
		assert.Equal(t, []string{"completed"}, r.query["status[]"])
		if r.query.Has("settled_at_from") {
			assert.Equal(t, "100", r.query.Get("per_page"))
			assert.Equal(t, "2026-09-30T22:00:00Z", r.query.Get("settled_at_from"))
			continue
		}
		assert.Equal(t, "8", r.query.Get("per_page"))
		assert.Equal(t, "settled_at:desc", r.query.Get("sort_by"))
	}
	assert.Equal(t, []string{
		"organization",
		"transactions " + idSASMain + " recent 1",
		"transactions " + idSASMain + " month 1",
		"transactions " + idSASMain + " month 2",
		"transactions " + idSASSavings + " recent 1",
		"transactions " + idSASSavings + " month 1",
	}, got, "inactive and non-EUR accounts are not queried")
}

func TestCollect_MonthBoundary(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
		want string
	}{
		{"00:30 on the 1st in Paris is already October", utc(9, 30, 22, 30, 0), "2026-09-30T22:00:00Z"},
		{"mid October", utc(10, 15, 12, 0, 0), "2026-09-30T22:00:00Z"},
		{"23:59 on the 31st, after the clocks went back", utc(10, 31, 22, 59, 0), "2026-09-30T22:00:00Z"},
		{"00:30 on November 1st in Paris", utc(10, 31, 23, 30, 0), "2026-10-31T23:00:00Z"},
		{"00:30 on April 1st, summer time", utc(3, 31, 22, 30, 0), "2026-03-31T22:00:00Z"},
		{"23:30 on March 31st, summer time", utc(3, 31, 21, 30, 0), "2026-02-28T23:00:00Z"},
		{"00:30 on January 1st", utc(12, 31, 23, 30, 0), "2026-12-31T23:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.now, ei(t))
			e.collect(t)
			var got []string
			for _, r := range e.f.requests(slugEI + ":" + secretEI) {
				if r.query.Has("settled_at_from") {
					got = append(got, r.query.Get("settled_at_from"))
				}
			}
			assert.Equal(t, []string{tt.want}, got)
		})
	}
}

func TestCollect_Warn(t *testing.T) {
	tests := []struct {
		warnBelow float64
		want      bool
	}{
		{0, false},
		{-100, false},
		{5000, false},
		{13980.31, false},
		{13980.32, false},
		{13980.33, true},
		{20000, true},
	}
	for _, tt := range tests {
		t.Run(strconv.FormatFloat(tt.warnBelow, 'f', -1, 64), func(t *testing.T) {
			e := newEnv(t, testNow, sas(t, tt.warnBelow))
			assert.Equal(t, tt.want, e.collect(t).Orgs[0].Warn)
		})
	}
}

func TestCollect_Unauthorized(t *testing.T) {
	e := newEnv(t, testNow, sas(t, 0).with(unauthorized(t)))
	got, err := e.c.Collect(t.Context())
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Equal(t, "every org failed: kOlapsis SAS: organization: HTTP 401: API key (slug:secret) rejected", err.Error())
	assert.NotContains(t, e.logs.String(), secretSAS)
}

func TestCollect_ErrorBodiesNeverEchoTheSecret(t *testing.T) {
	padding := strings.Repeat("x", 195)
	tests := []struct {
		name string
		body string
	}{
		{"whole credentials", `{"errors":[{"detail":"cannot parse ` + slugSAS + ":" + secretSAS + `"}]}`},
		{"secret alone", `{"errors":[{"detail":"unknown secret ` + secretSAS + `"}]}`},
		{"secret near the cut-off", padding + secretSAS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, testNow, sas(t, 0).with(respondRaw(http.StatusForbidden, tt.body)))
			_, err := e.c.Collect(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "organization: HTTP 403: ")
			assert.NotContains(t, err.Error(), "FAKEs")
			assert.NotContains(t, e.logs.String(), "FAKEs")
		})
	}
}

func TestCollect_EndpointFailures(t *testing.T) {
	tests := []struct {
		name   string
		sig    string
		status int
		body   string
		want   string
	}{
		{"organization behind a failing proxy", "organization", http.StatusBadGateway, "", "organization: HTTP 502"},
		{"organization that is not JSON", "organization", http.StatusOK, "<html>", "organization: decode: "},
		{"recent transactions", "transactions " + idSASMain + " recent 1", http.StatusTooManyRequests, `{"errors":[{"code":"rate_limit_exceeded"}]}`, `transactions: HTTP 429: {"errors":[{"code":"rate_limit_exceeded"}]}`},
		{"first page of the month", "transactions " + idSASMain + " month 1", http.StatusForbidden, `{"errors":[{"code":"forbidden","detail":"User does not have sufficient permissions for this action."}]}`, `transactions: HTTP 403: {"errors":[{"code":"forbidden","detail":"User does not have sufficient permissions for this action."}]}`},
		{"second page of the month", "transactions " + idSASMain + " month 2", http.StatusInternalServerError, "", "transactions: HTTP 500"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, testNow, sas(t, 0).with(failOn(tt.sig, tt.status, tt.body, sasRoutes().handler(t))))
			_, err := e.c.Collect(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "kOlapsis SAS: "+tt.want)
		})
	}
}

func TestScrubWithoutSecret(t *testing.T) {
	assert.Equal(t, "nothing to hide", client{slug: "slug"}.scrub("nothing to hide"))
}

func TestCollect_TransportErrorLeavesNoQueryString(t *testing.T) {
	e := newEnv(t, testNow, sas(t, 0).with(dropTransactions(sasRoutes().handler(t))))
	_, err := e.c.Collect(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kOlapsis SAS: transactions: EOF")
	assert.NotContains(t, err.Error(), "?")
	assert.NotContains(t, err.Error(), "bank_account_id")
	assert.NotContains(t, e.logs.String(), "bank_account_id")
}

func TestCollect_PartialFailureKeepsLastGoodOrgStale(t *testing.T) {
	e := newEnv(t, testNow, sas(t, 0), ei(t))
	first := e.collect(t)
	assert.False(t, first.Orgs[0].Stale)

	e.f.set(slugSAS+":"+secretSAS, serverError())
	second := e.collect(t)
	cached := second.Orgs[0]
	assert.True(t, cached.Stale)
	assert.Equal(t, `organization: HTTP 500: {"errors":[{"code":"internal_server_error","detail":"boom"}]}`, cached.Error)
	cached.Stale, cached.Error = false, ""
	assert.Equal(t, first.Orgs[0], cached, "values of the last good fetch are kept")
	assert.Equal(t, first.Orgs[1], second.Orgs[1])
	assert.Equal(t, first.TotalCents, second.TotalCents)
	assert.Len(t, second.Series30, 1, "a cached org still lets the total be recorded")

	e.f.set(slugSAS+":"+secretSAS, sasRoutes().handler(t))
	third := e.collect(t)
	assert.Equal(t, first.Orgs, third.Orgs)

	assert.Contains(t, e.logs.String(), "qonto org failed")
}

func TestCollect_OrgNeverFetchedIsReportedWithoutData(t *testing.T) {
	e := newEnv(t, testNow, sas(t, 0).with(unauthorized(t)), ei(t))
	got := e.collect(t)

	assert.Equal(t, Org{
		Name:     "kOlapsis SAS",
		Accounts: []Account{},
		Recent:   []Transaction{},
		Stale:    true,
		Error:    "organization: HTTP 401: API key (slug:secret) rejected",
	}, got.Orgs[0])
	assert.Equal(t, wantEI, got.Orgs[1])
	assert.Equal(t, wantEI.TotalCents, got.TotalCents)
	assert.Empty(t, got.Series30, "an incomplete total is not recorded")
	assert.NotNil(t, got.Series30)
}

func TestCollect_EveryOrgFailedAndNothingCached(t *testing.T) {
	e := newEnv(t, testNow, sas(t, 0).with(unauthorized(t)), ei(t).with(serverError()))
	got, err := e.c.Collect(t.Context())
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "kOlapsis SAS: organization: HTTP 401")
	assert.Contains(t, err.Error(), "EI: organization: HTTP 500")
	for _, secret := range []string{secretSAS, secretEI} {
		assert.NotContains(t, err.Error(), secret)
		assert.NotContains(t, e.logs.String(), secret)
	}
}

func TestCollect_EveryOrgFailedButCached(t *testing.T) {
	e := newEnv(t, testNow, sas(t, 0), ei(t))
	good := e.collect(t)

	e.f.set(slugSAS+":"+secretSAS, serverError())
	e.f.set(slugEI+":"+secretEI, unauthorized(t))
	got := e.collect(t)
	for _, o := range got.Orgs {
		assert.True(t, o.Stale, o.Name)
	}
	assert.Equal(t, good.TotalCents, got.TotalCents)
}

func TestCollect_History(t *testing.T) {
	e := newEnv(t, testNow, sas(t, 0), ei(t))
	first := e.collect(t)
	assert.Equal(t, []store.Point{{T: utc(10, 7, 8, 0, 0), V: 1719042}}, first.Series30)

	e.clk.Advance(2 * time.Hour)
	second := e.collect(t)
	assert.Equal(t, []store.Point{
		{T: utc(10, 7, 8, 0, 0), V: 1719042},
		{T: utc(10, 7, 10, 0, 0), V: 1719042},
	}, second.Series30)
}

type spyHistory struct {
	*store.Mem
	puts atomic.Int64
}

func (s *spyHistory) Put(ctx context.Context, col, key string, at time.Time, v float64) error {
	s.puts.Add(1)
	return s.Mem.Put(ctx, col, key, at, v)
}

func TestCollect_Backfill(t *testing.T) {
	backfilled := []store.Point{
		{T: utc(10, 2, 5, 0, 0), V: 100910},
		{T: utc(10, 3, 15, 0, 0), V: 321010},
		{T: utc(10, 7, 8, 0, 0), V: 321010},
	}

	t.Run("single org with a single account", func(t *testing.T) {
		e := newEnv(t, testNow, ei(t))
		spy := &spyHistory{Mem: e.hist}
		e.c.deps.Hist = spy

		first := e.collect(t)
		assert.Equal(t, backfilled, first.Series30, "last balance of each settled day, then the live total")
		assert.Equal(t, int64(3), spy.puts.Load(), "two settled days, then the live total")

		e.collect(t)
		assert.Equal(t, int64(4), spy.puts.Load(), "the second cycle only records the live total")
	})
	t.Run("history already reaches back 7 days", func(t *testing.T) {
		e := newEnv(t, testNow, ei(t))
		require.NoError(t, e.hist.Put(t.Context(), "qonto", "balance.total", testNow.Add(-7*24*time.Hour), 250000))
		got := e.collect(t)
		assert.Equal(t, []store.Point{{T: utc(9, 30, 8, 0, 0), V: 250000}, {T: utc(10, 7, 8, 0, 0), V: 321010}}, got.Series30)
	})
	t.Run("an org with several accounts is not backfilled", func(t *testing.T) {
		e := newEnv(t, testNow, sas(t, 0))
		got := e.collect(t)
		assert.Equal(t, []store.Point{{T: utc(10, 7, 8, 0, 0), V: 1398032}}, got.Series30)
	})
	t.Run("several orgs are not backfilled", func(t *testing.T) {
		e := newEnv(t, testNow, sas(t, 0), ei(t))
		got := e.collect(t)
		assert.Equal(t, []store.Point{{T: utc(10, 7, 8, 0, 0), V: 1719042}}, got.Series30)
	})
}

func TestBackfillSelection(t *testing.T) {
	balance := func(v int64) *int64 { return &v }
	c := New(config.Qonto{}, collector.Deps{Hist: store.NewMem(), Loc: paris})
	ledger := []transaction{
		{SettledAt: utc(10, 3, 6, 0, 0), SettledBalanceCents: balance(100)},
		{SettledAt: utc(10, 3, 14, 0, 0), SettledBalanceCents: balance(200)},
		{SettledAt: utc(10, 3, 9, 0, 0), SettledBalanceCents: balance(150)},
		{SettledAt: utc(10, 4, 9, 0, 0)},
		{SettledAt: utc(10, 5, 9, 0, 0), SettledBalanceCents: balance(300), Status: "pending"},
		{SettledBalanceCents: balance(400)},
		{SettledAt: utc(10, 3, 22, 30, 0), SettledBalanceCents: balance(500)},
	}
	c.backfill(t.Context(), ledger)

	pts, err := c.deps.Hist.Series(t.Context(), "qonto", "balance.total", utc(10, 1, 0, 0, 0), utc(10, 10, 0, 0, 0))
	require.NoError(t, err)
	assert.Equal(t, []store.Point{
		{T: utc(10, 3, 14, 0, 0), V: 200},
		{T: utc(10, 3, 22, 0, 0), V: 500},
	}, pts, "22:30 UTC on the 3rd is already the 4th in Paris, the 3rd keeps its 14:00 value")
}

func TestSumMonth(t *testing.T) {
	tests := []struct {
		name string
		txs  []transaction
		want Month
	}{
		{"nothing", nil, Month{}},
		{"credits and debits", []transaction{
			{Side: "credit", AmountCents: 1000},
			{Side: "debit", AmountCents: 250},
			{Side: "credit", AmountCents: 50},
			{Side: "debit", AmountCents: 5},
		}, Month{InCents: 1050, OutCents: 255}},
		{"pending and unknown sides are ignored", []transaction{
			{Side: "credit", AmountCents: 1000, Status: "completed"},
			{Side: "debit", AmountCents: 999, Status: "pending"},
			{Side: "credit", AmountCents: 777, Status: "declined"},
			{Side: "other", AmountCents: 11},
		}, Month{InCents: 1000}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sumMonth(tt.txs))
		})
	}
}

func TestTopRecent(t *testing.T) {
	var txs []transaction
	for i := range 12 {
		txs = append(txs, transaction{ID: strconv.Itoa(i), AmountCents: int64(i), SettledAt: utc(10, 1, i, 0, 0)})
	}
	txs = append(txs, transaction{ID: "pending", AmountCents: 99, Status: "pending", SettledAt: utc(10, 2, 0, 0, 0)})
	txs = append(txs, transaction{ID: "emitted-only", AmountCents: 77, EmittedAt: utc(10, 1, 10, 30, 0)})

	got := topRecent(txs)
	require.Len(t, got, recentCount)
	var amounts []int64
	for _, tx := range got {
		amounts = append(amounts, tx.AmountCents)
	}
	assert.Equal(t, []int64{11, 77, 10, 9, 8, 7, 6, 5}, amounts, "newest first, emitted_at when never settled, pending dropped")
}

func TestSettledSince_PaginationGuards(t *testing.T) {
	from := utc(10, 1, 0, 0, 0)

	t.Run("next_page that does not advance", func(t *testing.T) {
		e := newEnv(t, testNow, ei(t).with(respondRaw(http.StatusOK, `{"transactions":[],"meta":{"next_page":1}}`)))
		_, err := e.client().settledSince(t.Context(), idEIMain, from)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "next_page 1 after page 1")
	})
	t.Run("endless listing", func(t *testing.T) {
		var pages atomic.Int64
		e := newEnv(t, testNow, ei(t).with(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, []byte(`{"transactions":[],"meta":{"next_page":`+strconv.FormatInt(pages.Add(1)+1, 10)+`}}`))
		})))
		_, err := e.client().settledSince(t.Context(), idEIMain, from)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "more than 50 pages")
		assert.Equal(t, int64(maxPages), pages.Load())
	})
}

func TestSummary(t *testing.T) {
	c := New(config.Qonto{}, collector.Deps{})
	two := func(a, b Org) Data { return Data{TotalCents: 1569000, Orgs: []Org{a, b}} }
	month := Month{InCents: 269700, OutCents: 267700}

	tests := []struct {
		name string
		data any
		want string
	}{
		{"nominal", two(Org{Month: month}, Org{}), "15 690 € · 2 orgs · ce mois +2 697 / −2 677 €"},
		{"months add up across orgs", two(Org{Month: month}, Org{Month: month}), "15 690 € · 2 orgs · ce mois +5 394 / −5 354 €"},
		{"single org", Data{TotalCents: 99, Orgs: []Org{{}}}, "1 € · 1 org · ce mois +0 / −0 €"},
		{"overdrawn", Data{TotalCents: -123456, Orgs: []Org{{}}}, "−1 235 € · 1 org · ce mois +0 / −0 €"},
		{"failing org", two(Org{Month: month, Stale: true}, Org{}), "15 690 € · 2 orgs · ce mois +2 697 / −2 677 € (1 en erreur)"},
		{"foreign type", 42, ""},
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

func TestClientURLs(t *testing.T) {
	e := newEnv(t, testNow, ei(t))
	_, err := e.client().recent(t.Context(), idEIMain)
	require.NoError(t, err)
	reqs := e.f.requests(slugEI + ":" + secretEI)
	require.Len(t, reqs, 1)
	assert.Equal(t, url.Values{
		"bank_account_id": {idEIMain},
		"status[]":        {"completed"},
		"sort_by":         {"settled_at:desc"},
		"per_page":        {"8"},
	}, reqs[0].query)
}
