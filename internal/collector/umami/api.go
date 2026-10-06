package umami

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/kolapsis/pi-dashboard/internal/httpx"
)

func (c *Collector) get(ctx context.Context, path string, q url.Values, out any) error {
	tok, err := c.authToken(ctx)
	if err != nil {
		return err
	}
	err = c.fetch(ctx, path, q, tok, out)
	if !c.unauthorized(err) {
		return err
	}
	if tok, err = c.renewToken(ctx, tok); err != nil {
		return err
	}
	return c.fetch(ctx, path, q, tok, out)
}

func (c *Collector) fetch(ctx context.Context, path string, q url.Values, token string, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return redactErr(httpx.GetJSON(ctx, c.deps.HTTP, u, map[string]string{"Authorization": "Bearer " + token}, out))
}

func (c *Collector) unauthorized(err error) bool {
	var se *httpx.StatusError
	return c.cfg.APIKey == "" && errors.As(err, &se) && se.Status == http.StatusUnauthorized
}

func (c *Collector) authToken(ctx context.Context) (string, error) {
	if c.cfg.APIKey != "" {
		return c.cfg.APIKey, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" {
		return c.token, nil
	}
	return c.signIn(ctx)
}

func (c *Collector) renewToken(ctx context.Context, stale string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.token != stale {
		return c.token, nil
	}
	return c.signIn(ctx)
}

func (c *Collector) signIn(ctx context.Context) (string, error) {
	tok, err := c.login(ctx)
	c.token = tok
	return tok, err
}

func (c *Collector) login(ctx context.Context) (string, error) {
	payload, err := json.Marshal(map[string]string{"username": c.cfg.Username, "password": c.cfg.Password})
	if err != nil {
		return "", fmt.Errorf("login: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/auth/login", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("login: %w", redactErr(err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	body, _, err := httpx.Do(ctx, c.deps.HTTP, req)
	if err != nil {
		return "", fmt.Errorf("login: %w", redactErr(err))
	}
	var out struct {
		Token             string `json:"token"`
		RequiresTwoFactor bool   `json:"requiresTwoFactor"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("login: decode response: %w", err)
	}
	switch {
	case out.RequiresTwoFactor:
		return "", errors.New("login: two-factor authentication is enabled, use api_key instead")
	case out.Token == "":
		return "", errors.New("login: response carries no token")
	}
	return out.Token, nil
}

func redactErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s %s: %w", ue.Op, httpx.Redact(ue.URL), ue.Err)
	}
	return err
}
