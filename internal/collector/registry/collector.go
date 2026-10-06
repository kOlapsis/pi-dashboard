package registry

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/httpx"
)

const (
	defaultGHCRBase = "https://github.com"
	defaultHubBase  = "https://hub.docker.com"
	itemTimeout     = 15 * time.Second
	concurrency     = 4
	deltaSpan       = 7 * 24 * time.Hour
	perDayKeep      = 30
)

var (
	errLayoutChanged = errors.New("GHCR page layout changed: total downloads not found")

	totalRe = regexp.MustCompile(`Total downloads\s*</span>\s*<h3[^>]*\stitle="(\d[\d,]*)"`)
	tagRe   = regexp.MustCompile(`<[^<>]*\sdata-merge-count="\d+"[^<>]*>`)
	countRe = regexp.MustCompile(`\sdata-merge-count="(\d+)"`)
	dateRe  = regexp.MustCompile(`\sdata-date="(\d{4}-\d{2}-\d{2})"`)
)

type Collector struct {
	cfg      config.Registry
	deps     collector.Deps
	ghcrBase string
	hubBase  string

	mu   sync.Mutex
	last []*Item
}

func New(cfg config.Registry, deps collector.Deps) *Collector {
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	return &Collector{
		cfg:      cfg,
		deps:     deps,
		ghcrBase: defaultGHCRBase,
		hubBase:  defaultHubBase,
		last:     make([]*Item, len(cfg.Items)),
	}
}

func (c *Collector) Name() string { return "registry" }

func (c *Collector) Interval() time.Duration { return c.cfg.Interval }

func (c *Collector) Timeout() time.Duration {
	waves := (len(c.cfg.Items) + concurrency - 1) / concurrency
	return time.Duration(max(waves, 1))*itemTimeout + 5*time.Second
}

func (c *Collector) Collect(ctx context.Context) (any, error) {
	items := c.cfg.Items
	if len(items) == 0 {
		return nil, errors.New("no items configured")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	fetched := make([]Item, len(items))
	errs := make([]error, len(items))
	var g errgroup.Group
	g.SetLimit(concurrency)
	for i, it := range items {
		g.Go(func() error {
			fetched[i], errs[i] = c.fetch(ctx, it)
			return nil
		})
	}
	_ = g.Wait()

	now := c.deps.Clock.Now()
	failed := 0
	var first error
	for i, it := range items {
		if errs[i] != nil {
			failed++
			c.deps.Log.Warn("registry item failed", "item", it.Name, "kind", it.Kind, "err", errs[i])
			if first == nil {
				first = fmt.Errorf("%s: %w", it.Name, errs[i])
			}
			continue
		}
		c.track(ctx, &fetched[i], now)
		c.last[i] = &fetched[i]
	}
	if failed == len(items) {
		return nil, fmt.Errorf("every item failed, first: %w", first)
	}

	d := Data{Items: make([]Item, 0, len(items))}
	for _, it := range c.last {
		if it != nil {
			d.Items = append(d.Items, *it)
		}
	}
	return d, nil
}

func (c *Collector) track(ctx context.Context, item *Item, now time.Time) {
	key := "pulls." + item.Name
	total := float64(item.Total)
	if delta, ok := collector.Delta(ctx, c.deps.Hist, c.Name(), key, now, deltaSpan, total); ok {
		v := int64(math.Round(delta))
		item.Delta7 = &v
	}
	collector.Record(ctx, c.deps.Hist, c.deps.Log, c.Name(), key, now, total)
}

func (c *Collector) fetch(ctx context.Context, it config.RegistryItem) (Item, error) {
	ctx, cancel := context.WithTimeout(ctx, itemTimeout)
	defer cancel()
	switch it.Kind {
	case "ghcr":
		return c.fetchGHCR(ctx, it)
	case "dockerhub":
		return c.fetchDockerHub(ctx, it)
	}
	return Item{}, fmt.Errorf("unknown kind %q", it.Kind)
}

func (c *Collector) fetchGHCR(ctx context.Context, it config.RegistryItem) (Item, error) {
	repo := cmp.Or(it.Repo, it.Package)
	if !strings.Contains(repo, "/") {
		repo = it.Org + "/" + repo
	}
	page, err := url.JoinPath(c.ghcrBase, repo, "pkgs", "container", it.Package)
	if err != nil {
		return Item{}, errors.New("invalid ghcr url")
	}
	req, err := http.NewRequest(http.MethodGet, page, nil)
	if err != nil {
		return Item{}, errors.New("invalid ghcr url")
	}
	req.Header.Set("Accept", "text/html")
	body, _, err := httpx.Do(ctx, c.deps.HTTP, req)
	if err != nil {
		return Item{}, fmt.Errorf("fetch ghcr page: %w", err)
	}
	total, perDay, err := parseGHCR(body)
	if err != nil {
		return Item{}, err
	}
	return Item{Name: it.Name, Kind: it.Kind, Total: total, PerDay: perDay}, nil
}

func (c *Collector) fetchDockerHub(ctx context.Context, it config.RegistryItem) (Item, error) {
	repo, err := url.JoinPath(c.hubBase, "v2", "repositories", it.Repo)
	if err != nil {
		return Item{}, errors.New("invalid dockerhub url")
	}
	var r struct {
		PullCount *int64 `json:"pull_count"`
		StarCount int    `json:"star_count"`
	}
	if err := httpx.GetJSON(ctx, c.deps.HTTP, repo+"/", nil, &r); err != nil {
		return Item{}, fmt.Errorf("fetch dockerhub repository: %w", err)
	}
	if r.PullCount == nil {
		return Item{}, errors.New("pull_count missing from dockerhub response")
	}
	return Item{Name: it.Name, Kind: it.Kind, Total: *r.PullCount, Stars: r.StarCount}, nil
}

func parseGHCR(page []byte) (int64, []collector.DayPoint, error) {
	m := totalRe.FindSubmatch(page)
	if m == nil {
		return 0, nil, errLayoutChanged
	}
	total, err := strconv.ParseInt(strings.ReplaceAll(string(m[1]), ",", ""), 10, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: unreadable total %q", errLayoutChanged, m[1])
	}
	return total, parseSeries(page), nil
}

func parseSeries(page []byte) []collector.DayPoint {
	counts := map[string]float64{}
	for _, tag := range tagRe.FindAll(page, -1) {
		count, date := countRe.FindSubmatch(tag), dateRe.FindSubmatch(tag)
		if count == nil || date == nil {
			continue
		}
		n, err := strconv.ParseFloat(string(count[1]), 64)
		if err != nil {
			continue
		}
		counts[string(date[1])] = n
	}
	dates := slices.Sorted(maps.Keys(counts))
	dates = dates[max(len(dates)-perDayKeep, 0):]
	if len(dates) == 0 {
		return nil
	}
	out := make([]collector.DayPoint, len(dates))
	for i, d := range dates {
		out[i] = collector.DayPoint{D: d, V: counts[d]}
	}
	return out
}

func (c *Collector) Summary(data any) string {
	d, ok := data.(Data)
	if !ok {
		return ""
	}
	parts := make([]string, len(d.Items))
	for i, it := range d.Items {
		parts[i] = fmt.Sprintf("%s %s pulls", it.Name, grouped(it.Total, false))
		if it.Delta7 != nil {
			parts[i] += fmt.Sprintf(" (%s / 7 j)", grouped(*it.Delta7, true))
		}
	}
	return strings.Join(parts, " · ")
}

func grouped(n int64, signed bool) string {
	abs := uint64(n)
	if n < 0 {
		abs = -abs
	}
	digits := strconv.FormatUint(abs, 10)
	var b strings.Builder
	switch {
	case n < 0:
		b.WriteByte('-')
	case signed:
		b.WriteByte('+')
	}
	for i := range len(digits) {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteByte(digits[i])
	}
	return b.String()
}
