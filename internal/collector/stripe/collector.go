package stripe

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/store"
)

const (
	defaultBaseURL = "https://api.stripe.com"
	historyKey     = "mrr.total"
	historySpan    = 30 * 24 * time.Hour
	maxErrorLen    = 200
)

type Collector struct {
	cfg     config.Stripe
	deps    collector.Deps
	baseURL string

	mu     sync.Mutex
	good   []*Account
	labels map[string]string
}

func New(cfg config.Stripe, deps collector.Deps) *Collector {
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	if deps.Loc == nil {
		deps.Loc = time.UTC
	}
	return &Collector{
		cfg:     cfg,
		deps:    deps,
		baseURL: defaultBaseURL,
		good:    make([]*Account, len(cfg.Accounts)),
		labels:  map[string]string{},
	}
}

func (c *Collector) Name() string { return "stripe" }

func (c *Collector) Interval() time.Duration { return c.cfg.Interval }

func (c *Collector) Timeout() time.Duration { return 45 * time.Second }

// Collect queries every account in turn and substitutes the last good result, flagged stale, for those that fail.
func (c *Collector) Collect(ctx context.Context) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.deps.Clock.Now()
	data := Data{Currency: "eur", Accounts: make([]Account, 0, len(c.cfg.Accounts))}
	var failures []string
	missing := 0
	for i, ac := range c.cfg.Accounts {
		acc, err := c.fetch(ctx, ac, now)
		if err == nil {
			c.good[i] = &acc
		} else {
			c.deps.Log.Warn("stripe account failed", "account", ac.Name, "err", err)
			failures = append(failures, ac.Name+": "+err.Error())
			if c.good[i] == nil {
				missing++
			}
			acc = c.stale(i, ac.Name, err)
		}
		data.Accounts = append(data.Accounts, acc)
		data.Total.add(acc)
	}
	if missing > 0 && missing == len(c.cfg.Accounts) {
		return nil, fmt.Errorf("every account failed: %s", strings.Join(failures, "; "))
	}
	if missing == 0 {
		total := float64(data.Total.MRRCents)
		collector.Record(ctx, c.deps.Hist, c.deps.Log, c.Name(), historyKey, now, total)
		if d, ok := collector.Delta(ctx, c.deps.Hist, c.Name(), historyKey, now, historySpan, total); ok {
			cents := int64(math.Round(d))
			data.Total.MRRDelta30Cents = &cents
		}
	}
	data.Series30 = collector.Series(ctx, c.deps.Hist, c.Name(), historyKey, now, historySpan)
	if data.Series30 == nil {
		data.Series30 = []store.Point{}
	}
	return data, nil
}

func (c *Collector) stale(i int, name string, err error) Account {
	acc := Account{Name: name}
	if c.good[i] != nil {
		acc = *c.good[i]
	}
	acc.Stale = true
	acc.Error = clip(err.Error(), maxErrorLen)
	return acc
}

func (t *Totals) add(a Account) {
	t.MRRCents += a.MRRCents
	t.MonthNetCents += a.MonthNetCents
	t.Payments += a.Payments
	t.AvailableCents += a.AvailableCents
	t.PendingCents += a.PendingCents
}

func (c *Collector) Summary(data any) string {
	d, ok := data.(Data)
	if !ok {
		return ""
	}
	s := fmt.Sprintf("MRR %s € · mois %s € (%d %s) · %d %s",
		euros(d.Total.MRRCents), euros(d.Total.MonthNetCents),
		d.Total.Payments, plural(d.Total.Payments, "paiement", "paiements"),
		len(d.Accounts), plural(len(d.Accounts), "compte", "comptes"))
	failing := 0
	for _, a := range d.Accounts {
		if a.Stale {
			failing++
		}
	}
	if failing > 0 {
		s += fmt.Sprintf(" (%d en erreur)", failing)
	}
	return s
}
