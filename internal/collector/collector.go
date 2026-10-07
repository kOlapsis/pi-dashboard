package collector

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/store"
)

type Collector interface {
	Name() string
	Interval() time.Duration
	Collect(ctx context.Context) (any, error)
}

// Summarizer gives doctor a one-line description of a successful result.
type Summarizer interface {
	Summary(data any) string
}

// Timeouter overrides the scheduler's default per-collect timeout.
type Timeouter interface {
	Timeout() time.Duration
}

// Notifier returns one label per change between two successful collections that deserves attention.
type Notifier interface {
	Notify(prev, cur any) []string
}

type Deps struct {
	HTTP  *http.Client
	Clock clock.Clock
	Hist  store.History
	Log   *slog.Logger
	Loc   *time.Location
}

type DayPoint struct {
	D string  `json:"d"`
	V float64 `json:"v"`
}

// Delta returns value(now) - value(now-back) from history, or ok=false when no past point exists.
func Delta(ctx context.Context, h store.History, collector, key string, now time.Time, back time.Duration, current float64) (float64, bool) {
	if h == nil {
		return 0, false
	}
	past, ok, err := h.At(ctx, collector, key, now.Add(-back))
	if err != nil || !ok {
		return 0, false
	}
	return current - past, true
}

func Series(ctx context.Context, h store.History, collector, key string, now time.Time, span time.Duration) []store.Point {
	if h == nil {
		return nil
	}
	pts, err := h.Series(ctx, collector, key, now.Add(-span), now)
	if err != nil {
		return nil
	}
	return pts
}

func Record(ctx context.Context, h store.History, log *slog.Logger, collector, key string, now time.Time, v float64) {
	if h == nil {
		return
	}
	if err := h.Put(ctx, collector, key, now, v); err != nil && log != nil {
		log.Warn("history write failed", "collector", collector, "key", key, "err", err)
	}
}
