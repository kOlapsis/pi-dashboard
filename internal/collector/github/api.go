package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/httpx"
)

const (
	defaultBase  = "https://api.github.com"
	apiVersion   = "2022-11-28"
	acceptJSON   = "application/vnd.github+json"
	acceptStar   = "application/vnd.github.star+json"
	expiryHeader = "GitHub-Authentication-Token-Expiration"
)

type rateLimitError struct {
	reset time.Time
	retry time.Duration
}

func (e *rateLimitError) Error() string {
	switch {
	case !e.reset.IsZero():
		return "github: rate limit exceeded, resets at " + e.reset.Format("15:04")
	case e.retry > 0:
		return "github: rate limited, retry in " + e.retry.String()
	}
	return "github: rate limited"
}

func (c *Collector) get(ctx context.Context, rawURL, accept string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, redactErr(err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	body, resp, err := httpx.Do(ctx, c.deps.HTTP, req)
	if resp != nil {
		if exp, ok := parseExpiry(resp.Header.Get(expiryHeader)); ok {
			c.expires = &exp
		}
	}
	if err != nil {
		if resp != nil {
			if rl := c.rateLimit(resp); rl != nil {
				return nil, nil, rl
			}
		}
		return nil, nil, redactErr(err)
	}
	return body, resp.Header, nil
}

func (c *Collector) getJSON(ctx context.Context, rawURL, accept string, out any) (http.Header, error) {
	body, h, err := c.get(ctx, rawURL, accept)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return nil, fmt.Errorf("%s: decode: %w", httpx.Redact(rawURL), err)
	}
	return h, nil
}

func (c *Collector) rateLimit(resp *http.Response) error {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	h := resp.Header
	if h.Get("X-RateLimit-Remaining") == "0" {
		e := &rateLimitError{}
		if sec, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			e.reset = time.Unix(sec, 0).In(c.deps.Loc)
		}
		return e
	}
	if sec, err := strconv.Atoi(h.Get("Retry-After")); err == nil && sec > 0 {
		return &rateLimitError{retry: time.Duration(sec) * time.Second}
	}
	return nil
}

func (c *Collector) sameOrigin(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	b, err := url.Parse(c.base)
	if err != nil {
		return false
	}
	return u.Scheme == b.Scheme && u.Host == b.Host
}

func redactErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s %s: %w", ue.Op, httpx.Redact(ue.URL), ue.Err)
	}
	return err
}

var linkRE = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?([^",;\s]+)"?`)

func links(h http.Header) map[string]string {
	out := map[string]string{}
	for _, m := range linkRE.FindAllStringSubmatch(strings.Join(h.Values("Link"), ","), -1) {
		out[m[2]] = m[1]
	}
	return out
}

func lastPage(h http.Header) int {
	u, err := url.Parse(links(h)["last"])
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(u.Query().Get("page"))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

var expiryLayouts = []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700", time.RFC3339}

func parseExpiry(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range expiryLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
