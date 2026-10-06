package httpx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const MaxBody = 8 << 20

type StatusError struct {
	Status int
	URL    string
	Body   string
}

func (e *StatusError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("%s: HTTP %d: %s", e.URL, e.Status, e.Body)
	}
	return fmt.Sprintf("%s: HTTP %d", e.URL, e.Status)
}

func NewClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        16,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
}

// Redact strips user info and query values from a URL for logs and errors.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid url>"
	}
	u.User = nil
	if u.RawQuery != "" {
		u.RawQuery = ""
		u.Fragment = ""
		return u.String() + "?…"
	}
	return u.String()
}

// Do performs req, enforces the body limit and turns non-2xx into a StatusError.
func Do(ctx context.Context, c *http.Client, req *http.Request) ([]byte, *http.Response, error) {
	req = req.WithContext(ctx)
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "pi-dashboard (+https://github.com/kOlapsis/pi-dashboard)")
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	if err != nil {
		return nil, resp, fmt.Errorf("%s: read body: %w", Redact(req.URL.String()), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return body, resp, &StatusError{Status: resp.StatusCode, URL: Redact(req.URL.String()), Body: snippet(body)}
	}
	return body, resp, nil
}

func GetJSON(ctx context.Context, c *http.Client, rawURL string, headers map[string]string, out any) error {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	body, _, err := Do(ctx, c, req)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: decode: %w", Redact(rawURL), err)
	}
	return nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
