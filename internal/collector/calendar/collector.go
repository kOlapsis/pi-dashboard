package calendar

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/emersion/go-ical"
	"golang.org/x/sync/errgroup"

	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/httpx"
)

const (
	maxUpcoming = 6
	maxParallel = 4
)

var utf8BOM = []byte("\xef\xbb\xbf")

type Collector struct {
	cfg  config.Calendar
	deps collector.Deps
}

func New(cfg config.Calendar, deps collector.Deps) *Collector {
	return &Collector{cfg: cfg, deps: deps}
}

func (c *Collector) Name() string { return "calendar" }

func (c *Collector) Interval() time.Duration { return c.cfg.Interval }

func (c *Collector) Collect(ctx context.Context) (any, error) {
	feeds := c.cfg.Feeds
	if len(feeds) == 0 {
		return nil, errors.New("no calendar feeds configured")
	}
	w := newWindow(c.deps.Clock.Now(), c.cfg.LookaheadDays, c.deps.Loc)
	events := make([][]Event, len(feeds))
	errs := make([]error, len(feeds))
	var g errgroup.Group
	g.SetLimit(maxParallel)
	for i, f := range feeds {
		g.Go(func() error {
			events[i], errs[i] = c.load(ctx, f, w)
			return nil
		})
	}
	_ = g.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var all []Event
	var failed []string
	for i, f := range feeds {
		if errs[i] == nil {
			all = append(all, events[i]...)
			continue
		}
		label := cmp.Or(f.Name, fmt.Sprintf("#%d", i+1))
		c.deps.Log.Warn("calendar feed failed", "feed", label, "err", errs[i])
		failed = append(failed, fmt.Sprintf("%s: %v", label, errs[i]))
	}
	if len(failed) == len(feeds) {
		if len(feeds) == 1 {
			return nil, errors.New(failed[0])
		}
		return nil, fmt.Errorf("all %d calendar feeds failed: %s", len(feeds), strings.Join(failed, "; "))
	}
	return arrange(all, w), nil
}

func (c *Collector) load(ctx context.Context, f config.CalendarFeed, w window) (events []Event, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("parse feed: %v", r)
		}
	}()
	body, err := c.download(ctx, f.URL)
	if err != nil {
		return nil, err
	}
	cal, err := ical.NewDecoder(bytes.NewReader(body)).Decode()
	if err != nil {
		return nil, fmt.Errorf("parse feed: %w", err)
	}
	x := &expander{win: w, calendar: f.Name, overridden: map[occurrence]bool{}}
	events = x.events(cal)
	if x.skipped > 0 {
		c.deps.Log.Debug("calendar events skipped", "feed", f.Name, "count", x.skipped, "first_err", x.firstErr)
	}
	return events, nil
}

func (c *Collector) download(ctx context.Context, rawURL string) ([]byte, error) {
	if rest, ok := strings.CutPrefix(rawURL, "webcal://"); ok {
		rawURL = "https://" + rest
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, scrub(err, rawURL)
	}
	req.Header.Set("Accept", "text/calendar, */*;q=0.1")
	body, _, err := httpx.Do(ctx, c.deps.HTTP, req)
	if err != nil {
		return nil, scrub(err, rawURL)
	}
	if len(body) >= httpx.MaxBody {
		return nil, fmt.Errorf("feed exceeds %d MiB", httpx.MaxBody>>20)
	}
	return bytes.TrimPrefix(body, utf8BOM), nil
}

// scrub removes the feed URL from err: the secret of a private iCal address lives in its path.
func scrub(err error, rawURL string) error {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		return fmt.Errorf("HTTP %d", se.Status)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if msg := strings.ReplaceAll(err.Error(), httpx.Redact(rawURL), "feed"); msg != err.Error() {
		return errors.New(msg)
	}
	return err
}

func arrange(all []Event, w window) Data {
	today, upcoming := []Event{}, []Event{}
	for _, e := range all {
		if e.Start.Before(w.tomorrow) {
			today = append(today, e)
		} else {
			upcoming = append(upcoming, e)
		}
	}
	slices.SortStableFunc(today, func(a, b Event) int { return cmp.Or(allDayFirst(a, b), compare(a, b)) })
	slices.SortStableFunc(upcoming, compare)
	return Data{Today: today, Upcoming: upcoming[:min(len(upcoming), maxUpcoming)]}
}

func compare(a, b Event) int {
	return cmp.Or(
		a.Start.Compare(b.Start),
		allDayFirst(a, b),
		a.End.Compare(b.End),
		strings.Compare(a.Title, b.Title),
		strings.Compare(a.Calendar, b.Calendar),
	)
}

func allDayFirst(a, b Event) int {
	switch {
	case a.AllDay == b.AllDay:
		return 0
	case a.AllDay:
		return -1
	}
	return 1
}

func (c *Collector) Summary(data any) string {
	d, ok := data.(Data)
	if !ok {
		return ""
	}
	out := fmt.Sprintf("%d aujourd'hui", len(d.Today))
	now := c.deps.Clock.Now().In(c.deps.Loc)
	if next := nextEvent(d, now); next != nil {
		out += fmt.Sprintf(" · prochain : %s %s", next.Title, when(*next, now))
	}
	return out
}

func nextEvent(d Data, now time.Time) *Event {
	for i, e := range d.Today {
		if !e.AllDay && e.Start.After(now) {
			return &d.Today[i]
		}
	}
	if len(d.Upcoming) > 0 {
		return &d.Upcoming[0]
	}
	return nil
}

func when(e Event, now time.Time) string {
	start := e.Start.In(now.Location())
	switch {
	case e.AllDay:
		return start.Format("02/01")
	case start.YearDay() == now.YearDay() && start.Year() == now.Year():
		return start.Format("15:04")
	}
	return start.Format("02/01 15:04")
}
