package qonto

import (
	"context"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/collector"
)

func (c *Collector) backfill(ctx context.Context, ledger []transaction) {
	lastOfDay := map[string]transaction{}
	for _, tx := range ledger {
		if !tx.completed() || tx.SettledBalanceCents == nil || tx.SettledAt.IsZero() {
			continue
		}
		day := tx.SettledAt.In(c.deps.Loc).Format(time.DateOnly)
		if cur, ok := lastOfDay[day]; !ok || tx.SettledAt.After(cur.SettledAt) {
			lastOfDay[day] = tx
		}
	}
	for _, tx := range lastOfDay {
		collector.Record(ctx, c.deps.Hist, c.deps.Log, c.Name(), historyKey, tx.SettledAt, float64(*tx.SettledBalanceCents))
	}
}
