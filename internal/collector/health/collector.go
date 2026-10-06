package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/httpx"
)

const (
	checkTimeout = 10 * time.Second
	concurrency  = 5
	maxRedirects = 3
	maxBody      = 64 << 10
	userAgent    = "pi-dashboard (+https://github.com/kOlapsis/pi-dashboard)"
)

type Collector struct {
	cfg     config.Health
	deps    collector.Deps
	timeout time.Duration

	mu    sync.Mutex
	state []siteState
}

type siteState struct {
	up       bool
	failures int
}

type result struct {
	status   int
	latency  time.Duration
	certDays *int
	failure  string
}

func New(cfg config.Health, deps collector.Deps) *Collector {
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	state := make([]siteState, len(cfg.Sites))
	for i := range state {
		state[i].up = true
	}
	return &Collector{cfg: cfg, deps: deps, timeout: checkTimeout, state: state}
}

func (c *Collector) Name() string { return "health" }

func (c *Collector) Interval() time.Duration { return c.cfg.Interval }

func (c *Collector) Timeout() time.Duration {
	waves := (len(c.cfg.Sites) + concurrency - 1) / concurrency
	return time.Duration(max(waves, 1))*c.timeout + 5*time.Second
}

func (c *Collector) Collect(ctx context.Context) (any, error) {
	sites := c.cfg.Sites
	if len(sites) == 0 {
		return nil, errors.New("no sites configured")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	client := &http.Client{Transport: c.deps.HTTP.Transport, Timeout: c.timeout, CheckRedirect: limitRedirects}
	results := make([]result, len(sites))
	var g errgroup.Group
	g.SetLimit(concurrency)
	for i, s := range sites {
		g.Go(func() error {
			results[i] = c.check(ctx, client, s.URL)
			return nil
		})
	}
	_ = g.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	threshold := max(c.cfg.FailThreshold, 1)
	d := Data{Total: len(sites), Sites: make([]Site, len(sites))}
	for i, s := range sites {
		r, st := results[i], &c.state[i]
		wasUp := st.up
		if r.failure == "" {
			st.up, st.failures = true, 0
		} else {
			st.failures++
			st.up = st.up && st.failures < threshold
		}
		if st.up != wasUp {
			c.logTransition(s.Name, st.up, r.failure)
		}
		if st.up {
			d.Up++
		}
		d.Sites[i] = Site{
			Name:      s.Name,
			URL:       httpx.Redact(s.URL),
			Up:        st.up,
			Status:    r.status,
			LatencyMs: int(r.latency.Round(time.Millisecond) / time.Millisecond),
			CertDays:  r.certDays,
			Error:     r.failure,
		}
	}
	return d, nil
}

func (c *Collector) check(ctx context.Context, client *http.Client, rawURL string) result {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return result{failure: "invalid url"}
	}
	req.Header.Set("Accept", "text/html,*/*")
	req.Header.Set("User-Agent", userAgent)
	start := c.deps.Clock.Now()
	resp, err := client.Do(req)
	if err != nil {
		return result{failure: describe(err)}
	}
	now := c.deps.Clock.Now()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()

	r := result{status: resp.StatusCode, latency: now.Sub(start), certDays: certDays(resp, now)}
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		r.failure = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return r
}

func (c *Collector) logTransition(name string, up bool, reason string) {
	if up {
		c.deps.Log.Info("site is back up", "site", name)
		return
	}
	c.deps.Log.Warn("site is down", "site", name, "reason", reason)
}

func limitRedirects(_ *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	return nil
}

func certDays(resp *http.Response, now time.Time) *int {
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		return nil
	}
	days := daysUntil(resp.TLS.PeerCertificates[0].NotAfter, now)
	return &days
}

func daysUntil(notAfter, now time.Time) int {
	return int(math.Floor(notAfter.Sub(now).Hours() / 24))
}

func describe(err error) string {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return "timeout"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return err.Error()
}

func (c *Collector) Summary(data any) string {
	d, ok := data.(Data)
	if !ok {
		return ""
	}
	var slowest *Site
	var failing, down []string
	for i := range d.Sites {
		s := &d.Sites[i]
		switch {
		case !s.Up:
			down = append(down, fmt.Sprintf("%s (%s)", s.Name, s.Error))
		case s.Error != "":
			failing = append(failing, fmt.Sprintf("%s (%s)", s.Name, s.Error))
		case slowest == nil || s.LatencyMs > slowest.LatencyMs:
			slowest = s
		}
	}
	out := fmt.Sprintf("%d/%d up", d.Up, d.Total)
	if slowest != nil {
		out += fmt.Sprintf(", slowest %s %d ms", slowest.Name, slowest.LatencyMs)
	}
	if len(failing) > 0 {
		out += ", failing: " + strings.Join(failing, ", ")
	}
	if len(down) > 0 {
		out += ", down: " + strings.Join(down, ", ")
	}
	return out
}
