package umami

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
)

const (
	collectorName = "umami"
	statsWindow   = 7 * 24 * time.Hour
	siteWorkers   = 4
	barDays       = 14
)

// Collector reports visits, pageviews and live visitors of the websites of a self-hosted Umami.
type Collector struct {
	cfg  config.Umami
	deps collector.Deps
	base string

	mu    sync.Mutex
	token string
}

func New(cfg config.Umami, deps collector.Deps) *Collector {
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	return &Collector{cfg: cfg, deps: deps, base: strings.TrimRight(cfg.BaseURL, "/")}
}

func (c *Collector) Name() string { return collectorName }

func (c *Collector) Interval() time.Duration { return c.cfg.Interval }

func (c *Collector) Collect(ctx context.Context) (any, error) {
	now := c.deps.Clock.Now()
	refs, err := c.sites(ctx)
	if err != nil {
		return nil, err
	}
	days := c.days(now)

	found := make([]*Site, len(refs))
	failures := make([]error, len(refs))
	var g errgroup.Group
	g.SetLimit(siteWorkers)
	for i, ref := range refs {
		g.Go(func() error {
			s, err := c.collectSite(ctx, ref, now, days)
			if err != nil {
				failures[i] = err
				c.deps.Log.Warn("umami site skipped", "site", ref.name, "err", err)
				return nil
			}
			found[i] = &s
			return nil
		})
	}
	_ = g.Wait()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("collect sites: %w", err)
	}

	sites := make([]Site, 0, len(refs))
	for _, s := range found {
		if s != nil {
			sites = append(sites, *s)
		}
	}
	if len(sites) == 0 {
		return nil, fmt.Errorf("all %d sites failed, first error: %w", len(refs), firstError(failures))
	}
	slices.SortStableFunc(sites, func(a, b Site) int {
		return cmp.Or(cmp.Compare(b.Visits7, a.Visits7), cmp.Compare(a.Name, b.Name))
	})

	d := Data{Bars14: make([]collector.DayPoint, len(days)), Sites: sites}
	for i, day := range days {
		d.Bars14[i].D = day
	}
	for _, s := range sites {
		d.Visits7 += s.Visits7
		d.Prev7 += s.Prev7
		d.Live += s.Live
		for i, b := range s.Bars14 {
			d.Bars14[i].V += b.V
		}
	}
	d.DeltaPct = pct(d.Visits7, d.Prev7)
	return d, nil
}

func (c *Collector) Summary(data any) string {
	d, ok := data.(Data)
	if !ok {
		return ""
	}
	s := fmt.Sprintf("%d %s · %s %s / 7 j", len(d.Sites), plural(len(d.Sites), "site", "sites"), groupDigits(d.Visits7), plural(d.Visits7, "visite", "visites"))
	if d.DeltaPct != nil {
		s += fmt.Sprintf(" (%s %%)", strings.Replace(fmt.Sprintf("%+.1f", *d.DeltaPct), ".", ",", 1))
	}
	return fmt.Sprintf("%s · %d en direct", s, d.Live)
}

type siteRef struct{ id, name string }

func (c *Collector) sites(ctx context.Context) ([]siteRef, error) {
	var list websiteList
	if err := c.get(ctx, "/api/websites", url.Values{"pageSize": {"200"}}, &list); err != nil {
		return nil, fmt.Errorf("list websites: %w", err)
	}
	var refs []siteRef
	if len(c.cfg.Sites) == 0 {
		for _, w := range list {
			refs = append(refs, siteRef{id: w.ID, name: cmp.Or(w.Domain, w.Name, w.ID)})
		}
	}
	for _, want := range c.cfg.Sites {
		i := slices.IndexFunc(list, func(w website) bool { return strings.EqualFold(w.ID, want.ID) })
		if i < 0 {
			c.deps.Log.Warn("umami site not found", "id", want.ID)
			continue
		}
		w := list[i]
		refs = append(refs, siteRef{id: w.ID, name: cmp.Or(want.Label, w.Domain, w.Name, w.ID)})
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("no website to report among the %d listed", len(list))
	}
	return refs, nil
}

func (c *Collector) collectSite(ctx context.Context, ref siteRef, now time.Time, days []string) (Site, error) {
	s := Site{ID: ref.id, Name: ref.name}
	cur, err := c.stats(ctx, ref.id, now.Add(-statsWindow), now)
	if err != nil {
		return s, fmt.Errorf("stats: %w", err)
	}
	prev, err := c.stats(ctx, ref.id, now.Add(-2*statsWindow), now.Add(-statsWindow))
	if err != nil {
		return s, fmt.Errorf("previous stats: %w", err)
	}
	s.Visits7, s.Prev7, s.Pageviews7 = int(cur.Visits), int(prev.Visits), int(cur.Pageviews)
	s.DeltaPct = pct(s.Visits7, s.Prev7)
	if s.Bars14, err = c.bars(ctx, ref.id, now, days); err != nil {
		return s, fmt.Errorf("pageviews: %w", err)
	}
	if live, err := c.live(ctx, ref.id); err != nil {
		c.deps.Log.Warn("umami live visitors unavailable", "site", ref.name, "err", err)
	} else {
		s.Live = live
	}
	return s, nil
}

func (c *Collector) stats(ctx context.Context, id string, from, to time.Time) (stats, error) {
	var out stats
	q := url.Values{"startAt": {millis(from)}, "endAt": {millis(to)}}
	err := c.get(ctx, sitePath(id, "stats"), q, &out)
	return out, err
}

func (c *Collector) bars(ctx context.Context, id string, now time.Time, days []string) ([]collector.DayPoint, error) {
	q := url.Values{
		"startAt":  {millis(c.localTime(now, barDays-1, 0))},
		"endAt":    {millis(now)},
		"unit":     {"day"},
		"timezone": {c.deps.Loc.String()},
	}
	var out pageviewSeries
	if err := c.get(ctx, sitePath(id, "pageviews"), q, &out); err != nil {
		return nil, err
	}
	perDay := make(map[string]int, len(out.Sessions))
	for _, b := range out.Sessions {
		if day, ok := b.day(c.deps.Loc); ok {
			perDay[day] += int(b.Y)
		}
	}
	bars := make([]collector.DayPoint, len(days))
	for i, day := range days {
		bars[i] = collector.DayPoint{D: day, V: float64(perDay[day])}
	}
	return bars, nil
}

func (c *Collector) live(ctx context.Context, id string) (int, error) {
	var out active
	err := c.get(ctx, sitePath(id, "active"), nil, &out)
	return int(out), err
}

func (c *Collector) days(now time.Time) []string {
	out := make([]string, barDays)
	for i := range out {
		out[i] = c.localTime(now, barDays-1-i, 12).Format(dayLayout)
	}
	return out
}

// Calendar arithmetic, not 24h steps: a local day lasts 23 to 25 hours across DST changes.
func (c *Collector) localTime(now time.Time, daysBack, hour int) time.Time {
	y, m, d := now.In(c.deps.Loc).Date()
	return time.Date(y, m, d-daysBack, hour, 0, 0, 0, c.deps.Loc)
}

func sitePath(id, resource string) string {
	return "/api/websites/" + url.PathEscape(id) + "/" + resource
}

func millis(t time.Time) string { return fmt.Sprint(t.UnixMilli()) }

func firstError(errs []error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return errors.New("no error recorded")
}

func plural(n int, one, many string) string {
	if n <= 1 {
		return one
	}
	return many
}

func groupDigits(n int) string {
	s := fmt.Sprint(n)
	sign := ""
	if n < 0 {
		sign, s = "-", s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + " " + s[i:]
	}
	return sign + s
}
