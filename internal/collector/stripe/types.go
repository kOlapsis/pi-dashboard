package stripe

import (
	"time"

	"github.com/kolapsis/pi-dashboard/internal/store"
)

type Data struct {
	Currency string        `json:"currency"`
	Total    Totals        `json:"total"`
	Series30 []store.Point `json:"series30"`
	Accounts []Account     `json:"accounts"`
}

type Totals struct {
	MRRCents        int64  `json:"mrr_cents"`
	MRRDelta30Cents *int64 `json:"mrr_delta30_cents"`
	MonthNetCents   int64  `json:"month_net_cents"`
	Payments        int    `json:"payments"`
	AvailableCents  int64  `json:"available_cents"`
	PendingCents    int64  `json:"pending_cents"`
}

type Account struct {
	Name           string   `json:"name"`
	Livemode       bool     `json:"livemode"`
	MRRCents       int64    `json:"mrr_cents"`
	Subs           int      `json:"subs"`
	MeteredSubs    int      `json:"metered_subs,omitempty"`
	MonthNetCents  int64    `json:"month_net_cents"`
	Payments       int      `json:"payments"`
	AvailableCents int64    `json:"available_cents"`
	PendingCents   int64    `json:"pending_cents"`
	Last           *Payment `json:"last"`
	Stale          bool     `json:"stale,omitempty"`
	Error          string   `json:"error,omitempty"`
}

type Payment struct {
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	Label       string    `json:"label"`
	At          time.Time `json:"at"`
}
