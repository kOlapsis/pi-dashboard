package qonto

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/store"
)

const (
	defaultBaseURL = "https://thirdparty.qonto.com"
	historyKey     = "balance.total"
	historySpan    = 30 * 24 * time.Hour
	backfillProbe  = 7 * 24 * time.Hour
	maxErrorLen    = 200
)

type Collector struct {
	cfg     config.Qonto
	deps    collector.Deps
	baseURL string

	mu         sync.Mutex
	good       []*Org
	backfilled bool
}

func New(cfg config.Qonto, deps collector.Deps) *Collector {
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	if deps.Loc == nil {
		deps.Loc = time.UTC
	}
	return &Collector{cfg: cfg, deps: deps, baseURL: defaultBaseURL, good: make([]*Org, len(cfg.Orgs))}
}

func (c *Collector) Name() string { return "qonto" }

func (c *Collector) Interval() time.Duration { return c.cfg.Interval }

func (c *Collector) Timeout() time.Duration { return 45 * time.Second }

// Collect queries every organization in turn and substitutes the last good result, flagged stale, for those that fail.
func (c *Collector) Collect(ctx context.Context) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.deps.Clock.Now()
	since := monthStart(now, c.deps.Loc)
	data := Data{Orgs: make([]Org, 0, len(c.cfg.Orgs))}
	var failures []string
	var ledger []transaction
	missing := 0
	for i, oc := range c.cfg.Orgs {
		res, err := c.fetch(ctx, oc, since)
		org := res.org
		if err == nil {
			c.good[i] = &org
			if len(c.cfg.Orgs) == 1 {
				ledger = res.ledger
			}
		} else {
			c.deps.Log.Warn("qonto org failed", "org", oc.Name, "err", err)
			failures = append(failures, oc.Name+": "+err.Error())
			if c.good[i] == nil {
				missing++
			}
			org = c.stale(i, oc.Name, err)
		}
		data.Orgs = append(data.Orgs, org)
		data.TotalCents += org.TotalCents
	}
	if missing > 0 && missing == len(c.cfg.Orgs) {
		return nil, fmt.Errorf("every org failed: %s", strings.Join(failures, "; "))
	}
	if ledger != nil && !c.backfilled {
		c.backfilled = true
		if _, known := collector.Delta(ctx, c.deps.Hist, c.Name(), historyKey, now, backfillProbe, 0); !known {
			c.backfill(ctx, ledger)
		}
	}
	if missing == 0 {
		collector.Record(ctx, c.deps.Hist, c.deps.Log, c.Name(), historyKey, now, float64(data.TotalCents))
	}
	data.Series30 = collector.Series(ctx, c.deps.Hist, c.Name(), historyKey, now, historySpan)
	if data.Series30 == nil {
		data.Series30 = []store.Point{}
	}
	return data, nil
}

func (c *Collector) stale(i int, name string, err error) Org {
	org := Org{Name: name, Accounts: []Account{}, Recent: []Transaction{}}
	if c.good[i] != nil {
		org = *c.good[i]
	}
	org.Stale = true
	org.Error = clip(err.Error(), maxErrorLen)
	return org
}

func (c *Collector) Summary(data any) string {
	d, ok := data.(Data)
	if !ok {
		return ""
	}
	var in, out int64
	failing := 0
	for _, o := range d.Orgs {
		in += o.Month.InCents
		out += o.Month.OutCents
		if o.Stale {
			failing++
		}
	}
	s := fmt.Sprintf("%s € · %d %s · ce mois +%s / −%s €", euros(d.TotalCents), len(d.Orgs), plural(len(d.Orgs), "org", "orgs"), euros(in), euros(out))
	if failing > 0 {
		s += fmt.Sprintf(" (%d en erreur)", failing)
	}
	return s
}
