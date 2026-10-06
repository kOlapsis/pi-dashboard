package stripe

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/config"
)

func (c *Collector) fetch(ctx context.Context, ac config.StripeAccount, now time.Time) (Account, error) {
	cl := client{http: c.deps.HTTP, baseURL: c.baseURL, key: ac.Key}
	acc := Account{Name: ac.Name}
	if err := fetchBalance(ctx, cl, &acc); err != nil {
		return Account{}, err
	}
	if err := fetchSubscriptions(ctx, cl, &acc); err != nil {
		return Account{}, err
	}
	last, err := fetchMonth(ctx, cl, &acc, monthStart(now, c.deps.Loc))
	if err != nil {
		return Account{}, err
	}
	if last == nil {
		if last, err = fetchLastCharge(ctx, cl); err != nil {
			return Account{}, err
		}
	}
	if last != nil {
		acc.Last = &Payment{
			AmountCents: last.net(),
			Currency:    last.Currency,
			Label:       c.label(ctx, cl, ac.Name, *last),
			At:          time.Unix(last.Created, 0).UTC(),
		}
	}
	return acc, nil
}

func fetchBalance(ctx context.Context, cl client, acc *Account) error {
	var b balance
	if err := cl.get(ctx, "/v1/balance", nil, &b); err != nil {
		return err
	}
	acc.Livemode = b.Livemode
	acc.AvailableCents = eurTotal(b.Available)
	acc.PendingCents = eurTotal(b.Pending)
	return nil
}

func eurTotal(lines []moneyAmount) int64 {
	var total int64
	for _, l := range lines {
		if isEUR(l.Currency) {
			total += l.Amount
		}
	}
	return total
}

func fetchSubscriptions(ctx context.Context, cl client, acc *Account) error {
	seen := map[string]bool{}
	for _, status := range []string{"active", "past_due"} {
		err := listAll(ctx, cl, "/v1/subscriptions", url.Values{"status": {status}},
			func(s subscription) string { return s.ID },
			func(s subscription) {
				if seen[s.ID] {
					return
				}
				seen[s.ID] = true
				cents, priced, metered := s.mrr()
				acc.MRRCents += cents
				if priced {
					acc.Subs++
				}
				if metered {
					acc.MeteredSubs++
				}
			})
		if err != nil {
			return err
		}
	}
	return nil
}

func fetchMonth(ctx context.Context, cl client, acc *Account, start time.Time) (*charge, error) {
	var last *charge
	q := url.Values{"created[gte]": {strconv.FormatInt(start.Unix(), 10)}}
	err := listAll(ctx, cl, "/v1/charges", q,
		func(ch charge) string { return ch.ID },
		func(ch charge) {
			if !ch.succeeded() {
				return
			}
			if isEUR(ch.Currency) {
				acc.MonthNetCents += ch.net()
				acc.Payments++
			}
			if last == nil || ch.Created > last.Created {
				last = &ch
			}
		})
	if err != nil {
		return nil, err
	}
	return last, nil
}

func fetchLastCharge(ctx context.Context, cl client) (*charge, error) {
	var page struct {
		Data []charge `json:"data"`
	}
	if err := cl.get(ctx, "/v1/charges", url.Values{"limit": {"5"}}, &page); err != nil {
		return nil, err
	}
	if i := slices.IndexFunc(page.Data, charge.succeeded); i >= 0 {
		return &page.Data[i], nil
	}
	return nil, nil
}

func (c *Collector) label(ctx context.Context, cl client, account string, ch charge) string {
	if d := strings.TrimSpace(ch.Description); d != "" {
		return d
	}
	if ch.Invoice != "" {
		if l := c.invoiceLine(ctx, cl, account, ch); l != "" {
			return l
		}
	}
	return cmp.Or(ch.StatementDescriptor, ch.CalculatedStatementDescriptor, "Paiement")
}

func (c *Collector) invoiceLine(ctx context.Context, cl client, account string, ch charge) string {
	if l, ok := c.labels[ch.ID]; ok {
		return l
	}
	var inv invoice
	err := cl.get(ctx, "/v1/invoices/"+url.PathEscape(ch.Invoice), nil, &inv)
	if err != nil {
		c.deps.Log.Info("stripe invoice label unavailable", "account", account, "err", err)
		if !definitive(err) {
			return ""
		}
	}
	line := ""
	if len(inv.Lines.Data) > 0 {
		line = strings.TrimSpace(inv.Lines.Data[0].Description)
	}
	c.labels[ch.ID] = line
	return line
}

func definitive(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && (ae.status == http.StatusForbidden || ae.status == http.StatusNotFound)
}

func monthStart(now time.Time, loc *time.Location) time.Time {
	n := now.In(loc)
	return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
}

func isEUR(currency string) bool { return strings.EqualFold(currency, "eur") }
