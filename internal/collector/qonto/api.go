package qonto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/httpx"
)

const (
	recentCount = 8
	monthPage   = 100
	maxPages    = 50
)

type client struct {
	http    *http.Client
	baseURL string
	slug    string
	secret  string
}

func (cl client) get(ctx context.Context, path string, q url.Values, out any) error {
	endpoint := strings.TrimPrefix(path, "/v2/")
	target := cl.baseURL + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", endpoint, err)
	}
	req.Header.Set("Authorization", cl.slug+":"+cl.secret)
	req.Header.Set("Accept", "application/json")
	body, _, err := httpx.Do(ctx, cl.http, req)
	if err != nil {
		return cl.requestError(endpoint, err, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: decode: %w", endpoint, err)
	}
	return nil
}

func (cl client) requestError(endpoint string, err error, body []byte) error {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		msg := clip(strings.Join(strings.Fields(cl.scrub(string(body))), " "), maxErrorLen)
		if se.Status == http.StatusUnauthorized {
			msg = "API key (slug:secret) rejected"
		}
		if msg == "" {
			return fmt.Errorf("%s: HTTP %d", endpoint, se.Status)
		}
		return fmt.Errorf("%s: HTTP %d: %s", endpoint, se.Status, msg)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err // *url.Error carries the full URL, query included
	}
	return fmt.Errorf("%s: %w", endpoint, err)
}

func (cl client) scrub(s string) string {
	if cl.secret == "" {
		return s
	}
	s = strings.ReplaceAll(s, cl.slug+":"+cl.secret, "<secret>")
	return strings.ReplaceAll(s, cl.secret, "<secret>")
}

func (cl client) organization(ctx context.Context) ([]bankAccount, error) {
	var resp struct {
		Organization struct {
			BankAccounts []bankAccount `json:"bank_accounts"`
		} `json:"organization"`
	}
	if err := cl.get(ctx, "/v2/organization", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Organization.BankAccounts, nil
}

func (cl client) recent(ctx context.Context, accountID string) ([]transaction, error) {
	q := transactionQuery(accountID)
	q.Set("sort_by", "settled_at:desc")
	q.Set("per_page", strconv.Itoa(recentCount))
	var resp transactionsResponse
	if err := cl.get(ctx, "/v2/transactions", q, &resp); err != nil {
		return nil, err
	}
	return resp.Transactions, nil
}

func (cl client) settledSince(ctx context.Context, accountID string, from time.Time) ([]transaction, error) {
	q := transactionQuery(accountID)
	q.Set("settled_at_from", from.UTC().Format(time.RFC3339))
	q.Set("per_page", strconv.Itoa(monthPage))
	var all []transaction
	page := 1
	for range maxPages {
		q.Set("page", strconv.Itoa(page))
		var resp transactionsResponse
		if err := cl.get(ctx, "/v2/transactions", q, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Transactions...)
		if resp.Meta.NextPage == nil {
			return all, nil
		}
		if *resp.Meta.NextPage <= page {
			return nil, fmt.Errorf("transactions: next_page %d after page %d", *resp.Meta.NextPage, page)
		}
		page = *resp.Meta.NextPage
	}
	return nil, fmt.Errorf("transactions: more than %d pages", maxPages)
}

func transactionQuery(accountID string) url.Values {
	return url.Values{"bank_account_id": {accountID}, "status[]": {"completed"}}
}

type bankAccount struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	Status                 string `json:"status"`
	Main                   bool   `json:"main"`
	Currency               string `json:"currency"`
	BalanceCents           int64  `json:"balance_cents"`
	AuthorizedBalanceCents int64  `json:"authorized_balance_cents"`
}

type transactionsResponse struct {
	Transactions []transaction `json:"transactions"`
	Meta         struct {
		NextPage *int `json:"next_page"`
	} `json:"meta"`
}

type transaction struct {
	ID                    string    `json:"transaction_id"`
	AmountCents           int64     `json:"amount_cents"`
	Side                  string    `json:"side"`
	Label                 string    `json:"label"`
	CleanCounterpartyName string    `json:"clean_counterparty_name"`
	OperationType         string    `json:"operation_type"`
	Status                string    `json:"status"`
	SettledAt             time.Time `json:"settled_at"`
	EmittedAt             time.Time `json:"emitted_at"`
	SettledBalanceCents   *int64    `json:"settled_balance_cents"`
}

func (tx transaction) completed() bool { return tx.Status == "" || tx.Status == "completed" }

func (tx transaction) at() time.Time {
	if tx.SettledAt.IsZero() {
		return tx.EmittedAt.UTC()
	}
	return tx.SettledAt.UTC()
}
