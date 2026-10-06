package github

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/store"
)

const (
	collectorName = "github"
	keyTotal      = "stars.total"
	week          = 7 * 24 * time.Hour
	month         = 30 * 24 * time.Hour
)

// Collector reports GitHub stars, repository activity and the 14-day traffic of selected repositories.
type Collector struct {
	cfg  config.GitHub
	deps collector.Deps
	base string

	mu            sync.Mutex
	traffic       map[string]*trafficEntry
	backfilled    bool
	backfillTries int
	expires       *time.Time
}

func New(cfg config.GitHub, deps collector.Deps) *Collector {
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	return &Collector{cfg: cfg, deps: deps, base: defaultBase, traffic: map[string]*trafficEntry{}}
}

func (c *Collector) Name() string { return collectorName }

func (c *Collector) Interval() time.Duration { return c.cfg.Interval }

// Timeout leaves room for the one-off stargazer backfill.
func (c *Collector) Timeout() time.Duration { return 90 * time.Second }

func (c *Collector) Collect(ctx context.Context) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.deps.Clock.Now()

	repos, err := c.repos(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]Repo, len(repos))
	total := 0
	for i, r := range repos {
		prs, err := c.openPRs(ctx, r)
		if err != nil {
			return nil, fmt.Errorf("%s: open pull requests: %w", r.full(), err)
		}
		rows[i] = Repo{
			Name: r.label, Stars: r.stars, Forks: r.forks, PRs: prs,
			Issues: max(r.openIssues-prs, 0), PushedAt: r.pushedAt,
		}
		if c.wantsTraffic(r) {
			t, err := c.trafficFor(ctx, r, now)
			if err != nil {
				return nil, fmt.Errorf("%s: traffic: %w", r.full(), err)
			}
			if t.valid {
				views, clones := t.views, t.clones
				rows[i].Views14, rows[i].Clones14 = &views, &clones
			}
		}
		total += r.stars
	}

	if err := c.backfill(ctx, repos, now); err != nil {
		c.deps.Log.Warn("github star history backfill failed", "attempt", c.backfillTries, "of", maxBackfillAttempts, "err", err)
	}

	for _, row := range rows {
		collector.Record(ctx, c.deps.Hist, c.deps.Log, collectorName, starsKey(row.Name), now, float64(row.Stars))
	}
	collector.Record(ctx, c.deps.Hist, c.deps.Log, collectorName, keyTotal, now, float64(total))
	for i := range rows {
		rows[i].Delta7 = c.delta7(ctx, starsKey(rows[i].Name), now, rows[i].Stars)
	}

	d := Data{
		Stars:    total,
		Delta7:   c.delta7(ctx, keyTotal, now, total),
		Series30: collector.Series(ctx, c.deps.Hist, collectorName, keyTotal, now, month),
		Repos:    rows,
	}
	if d.Series30 == nil {
		d.Series30 = []store.Point{}
	}
	if c.expires != nil {
		exp := *c.expires
		d.TokenExpiresAt = &exp
	}
	return d, nil
}

func (c *Collector) Summary(data any) string {
	d, ok := data.(Data)
	if !ok {
		return ""
	}
	s := groupDigits(d.Stars) + " ★"
	if d.Delta7 != nil {
		s += fmt.Sprintf(" (%+d / 7 j)", *d.Delta7)
	}
	n := len(d.Repos)
	return fmt.Sprintf("%s · %d %s", s, n, plural(n, "repo", "repos"))
}

func (c *Collector) delta7(ctx context.Context, key string, now time.Time, current int) *int {
	v, ok := collector.Delta(ctx, c.deps.Hist, collectorName, key, now, week, float64(current))
	if !ok {
		return nil
	}
	d := int(math.Round(v))
	return &d
}

func starsKey(label string) string { return "stars." + label }

func plural(n int, one, many string) string {
	if n <= 1 {
		return one
	}
	return many
}

func groupDigits(n int) string {
	s := fmt.Sprintf("%d", n)
	sign := ""
	if n < 0 {
		sign, s = "-", s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + " " + s[i:]
	}
	return sign + s
}
