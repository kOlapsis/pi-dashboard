package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/httpx"
)

type trafficEntry struct {
	views, clones int
	valid         bool
	denied        bool
	at            time.Time
}

func (c *Collector) trafficFor(ctx context.Context, r repo, now time.Time) (*trafficEntry, error) {
	key := strings.ToLower(r.full())
	e := c.traffic[key]
	if e == nil {
		e = &trafficEntry{}
		c.traffic[key] = e
	}
	if !e.at.IsZero() && now.Sub(e.at) < c.cfg.TrafficInterval {
		return e, nil
	}
	views, err := c.trafficCount(ctx, r, "views")
	var clones int
	if err == nil {
		clones, err = c.trafficCount(ctx, r, "clones")
	}
	var (
		rl *rateLimitError
		se *httpx.StatusError
	)
	switch {
	case err == nil:
		*e = trafficEntry{views: views, clones: clones, valid: true, at: now}
	case errors.As(err, &rl):
		return nil, err
	case errors.As(err, &se) && se.Status == http.StatusForbidden:
		if !e.denied {
			c.deps.Log.Warn("github traffic unavailable, the token needs Administration: read", "repo", r.full())
		}
		*e = trafficEntry{denied: true, at: now}
	default:
		c.deps.Log.Warn("github traffic failed", "repo", r.full(), "err", err)
	}
	return e, nil
}

func (c *Collector) trafficCount(ctx context.Context, r repo, kind string) (int, error) {
	var out struct {
		Count int `json:"count"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/traffic/%s", c.base, url.PathEscape(r.owner), url.PathEscape(r.name), kind)
	if _, err := c.getJSON(ctx, u, acceptJSON, &out); err != nil {
		return 0, err
	}
	return out.Count, nil
}
