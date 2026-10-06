package stripe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/kolapsis/pi-dashboard/internal/httpx"
)

const (
	apiVersion = "2024-06-20" // pinned: charge.invoice, read for the label, is gone from 2025-03-31.basil
	pageSize   = "100"
	maxPages   = 50
)

var (
	keyPattern        = regexp.MustCompile(`[rs]k_(?:live|test)_[A-Za-z0-9*]+`)
	permissionPattern = regexp.MustCompile(`'(rak_[a-z0-9_]+)'`)
)

type client struct {
	http    *http.Client
	baseURL string
	key     string
}

type apiError struct {
	endpoint string
	status   int
	message  string
}

func (e *apiError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("%s: HTTP %d", e.endpoint, e.status)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.endpoint, e.status, e.message)
}

func (cl client) get(ctx context.Context, path string, q url.Values, out any) error {
	endpoint := endpointName(path)
	target := cl.baseURL + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+cl.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Stripe-Version", apiVersion)
	body, _, err := httpx.Do(ctx, cl.http, req)
	if err != nil {
		return requestError(endpoint, err, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: decode: %w", endpoint, err)
	}
	return nil
}

func requestError(endpoint string, err error, body []byte) error {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		return &apiError{endpoint: endpoint, status: se.Status, message: apiMessage(body)}
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err // *url.Error carries the full URL, query included
	}
	return fmt.Errorf("%s: %w", endpoint, err)
}

func apiMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error.Message == "" {
		return ""
	}
	msg := e.Error.Message
	if m := permissionPattern.FindStringSubmatch(msg); m != nil {
		return "missing permission " + m[1]
	}
	return clip(keyPattern.ReplaceAllString(msg, "<key>"), maxErrorLen)
}

func endpointName(path string) string {
	name, _, _ := strings.Cut(strings.TrimPrefix(path, "/v1/"), "/")
	return name
}

func listAll[T any](ctx context.Context, cl client, path string, q url.Values, id func(T) string, each func(T)) error {
	q = maps.Clone(q)
	q.Set("limit", pageSize)
	for range maxPages {
		var page struct {
			Data    []T  `json:"data"`
			HasMore bool `json:"has_more"`
		}
		if err := cl.get(ctx, path, q, &page); err != nil {
			return err
		}
		for _, it := range page.Data {
			each(it)
		}
		if !page.HasMore {
			return nil
		}
		if len(page.Data) == 0 {
			return fmt.Errorf("%s: has_more set on an empty page", endpointName(path))
		}
		q.Set("starting_after", id(page.Data[len(page.Data)-1]))
	}
	return fmt.Errorf("%s: more than %d pages", endpointName(path), maxPages)
}

type balance struct {
	Livemode  bool          `json:"livemode"`
	Available []moneyAmount `json:"available"`
	Pending   []moneyAmount `json:"pending"`
}

type moneyAmount struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type subscription struct {
	ID    string `json:"id"`
	Items struct {
		Data []subscriptionItem `json:"data"`
	} `json:"items"`
}

type subscriptionItem struct {
	Quantity *int64 `json:"quantity"`
	Price    struct {
		Currency   string `json:"currency"`
		UnitAmount *int64 `json:"unit_amount"`
		Recurring  *struct {
			Interval      string `json:"interval"`
			IntervalCount int64  `json:"interval_count"`
			UsageType     string `json:"usage_type"`
		} `json:"recurring"`
	} `json:"price"`
}

type charge struct {
	ID                            string `json:"id"`
	Amount                        int64  `json:"amount"`
	AmountRefunded                int64  `json:"amount_refunded"`
	Currency                      string `json:"currency"`
	Created                       int64  `json:"created"`
	Description                   string `json:"description"`
	Invoice                       string `json:"invoice"`
	Paid                          bool   `json:"paid"`
	Status                        string `json:"status"`
	StatementDescriptor           string `json:"statement_descriptor"`
	CalculatedStatementDescriptor string `json:"calculated_statement_descriptor"`
}

func (ch charge) succeeded() bool { return ch.Status == "succeeded" && ch.Paid }

func (ch charge) net() int64 { return ch.Amount - ch.AmountRefunded }

type invoice struct {
	Lines struct {
		Data []struct {
			Description string `json:"description"`
		} `json:"data"`
	} `json:"lines"`
}
