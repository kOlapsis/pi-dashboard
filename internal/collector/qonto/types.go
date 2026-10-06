package qonto

import (
	"time"

	"github.com/kolapsis/pi-dashboard/internal/store"
)

type Data struct {
	TotalCents int64         `json:"total_cents"`
	Series30   []store.Point `json:"series30"`
	Orgs       []Org         `json:"orgs"`
}

type Org struct {
	Name       string        `json:"name"`
	TotalCents int64         `json:"total_cents"`
	Warn       bool          `json:"warn"`
	Accounts   []Account     `json:"accounts"`
	Month      Month         `json:"month"`
	Recent     []Transaction `json:"recent"`
	Stale      bool          `json:"stale,omitempty"`
	Error      string        `json:"error,omitempty"`
}

type Account struct {
	Name            string `json:"name"`
	BalanceCents    int64  `json:"balance_cents"`
	AuthorizedCents int64  `json:"authorized_cents"`
	Main            bool   `json:"main"`
}

type Month struct {
	InCents  int64 `json:"in_cents"`
	OutCents int64 `json:"out_cents"`
}

type Transaction struct {
	Label        string    `json:"label"`
	Counterparty string    `json:"counterparty,omitempty"`
	AmountCents  int64     `json:"amount_cents"`
	Side         string    `json:"side"`
	Type         string    `json:"type,omitempty"`
	At           time.Time `json:"at"`
}
