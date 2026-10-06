package github

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/collector"
)

const (
	backfillDays     = 90
	maxBackfillStars = 5000
	maxStarPages     = 50
	starsPerPage     = 100

	maxBackfillAttempts = 3
)

type stargazer struct {
	StarredAt time.Time `json:"starred_at"`
}

func (c *Collector) backfill(ctx context.Context, repos []repo, now time.Time) error {
	if c.backfilled || c.backfillTries >= maxBackfillAttempts || c.deps.Hist == nil {
		return nil
	}
	if c.cfg.Token == "" {
		c.backfilled = true
		c.deps.Log.Info("github star history not backfilled, listing stargazers needs a token")
		return nil
	}
	_, found, err := c.deps.Hist.At(ctx, collectorName, keyTotal, now.Add(-week))
	if err != nil {
		return fmt.Errorf("read history: %w", err)
	}
	if found {
		c.backfilled = true
		return nil
	}
	c.backfillTries++

	days := c.midnights(now)
	counts := make(map[string][]int, len(repos))
	complete := true
	pages := 0
	for _, r := range repos {
		if r.stars > maxBackfillStars {
			complete = false
			continue
		}
		times, n, err := c.stargazers(ctx, r)
		pages += n
		if err != nil {
			return fmt.Errorf("%s: stargazers: %w", r.full(), err)
		}
		counts[r.label] = cumulative(times, days)
	}

	totals := make([]int, len(days))
	for label, cs := range counts {
		for i, n := range cs {
			totals[i] += n
		}
		for i, day := range days {
			collector.Record(ctx, c.deps.Hist, c.deps.Log, collectorName, starsKey(label), day, float64(cs[i]))
		}
	}
	if complete {
		for i, day := range days {
			collector.Record(ctx, c.deps.Hist, c.deps.Log, collectorName, keyTotal, day, float64(totals[i]))
		}
	} else {
		c.deps.Log.Warn("github star history not backfilled for the total, a repository has more than the supported stars", "max_stars", maxBackfillStars)
	}
	c.backfilled = true
	c.deps.Log.Info("github star history backfilled", "repos", len(counts), "pages", pages, "days", len(days))
	return nil
}

func (c *Collector) midnights(now time.Time) []time.Time {
	y, m, d := now.In(c.deps.Loc).Date()
	out := make([]time.Time, backfillDays)
	for i := range out {
		out[i] = time.Date(y, m, d-(backfillDays-1-i), 0, 0, 0, 0, c.deps.Loc)
	}
	return out
}

func (c *Collector) stargazers(ctx context.Context, r repo) ([]time.Time, int, error) {
	if r.stars == 0 {
		return nil, 0, nil
	}
	var (
		out   []time.Time
		pages int
	)
	for page := 1; page <= maxStarPages; page++ {
		u := fmt.Sprintf("%s/repos/%s/%s/stargazers?per_page=%d&page=%d", c.base, url.PathEscape(r.owner), url.PathEscape(r.name), starsPerPage, page)
		var items []stargazer
		if _, err := c.getJSON(ctx, u, acceptStar, &items); err != nil {
			return nil, pages, err
		}
		pages++
		for _, it := range items {
			out = append(out, it.StarredAt)
		}
		if len(items) < starsPerPage {
			break
		}
	}
	slices.SortFunc(out, time.Time.Compare)
	return out, pages, nil
}

func cumulative(times, days []time.Time) []int {
	out := make([]int, len(days))
	n := 0
	for i, day := range days {
		for n < len(times) && !times[n].After(day) {
			n++
		}
		out[i] = n
	}
	return out
}
