package qonto

import (
	"context"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/config"
)

type fetched struct {
	org    Org
	ledger []transaction
}

func (c *Collector) fetch(ctx context.Context, oc config.QontoOrg, since time.Time) (fetched, error) {
	cl := client{http: c.deps.HTTP, baseURL: c.baseURL, slug: oc.Slug, secret: oc.Secret}
	accounts, err := cl.organization(ctx)
	if err != nil {
		return fetched{}, err
	}
	org := Org{Name: oc.Name, Accounts: []Account{}, Recent: []Transaction{}}
	var recent, ledger []transaction
	for _, ba := range accounts {
		if ba.Status != "active" || !strings.EqualFold(ba.Currency, "EUR") {
			continue
		}
		org.Accounts = append(org.Accounts, Account{Name: ba.Name, BalanceCents: ba.BalanceCents, AuthorizedCents: ba.AuthorizedBalanceCents, Main: ba.Main})
		org.TotalCents += ba.BalanceCents
		latest, err := cl.recent(ctx, ba.ID)
		if err != nil {
			return fetched{}, err
		}
		month, err := cl.settledSince(ctx, ba.ID, since)
		if err != nil {
			return fetched{}, err
		}
		recent = append(recent, latest...)
		ledger = slices.Concat(ledger, month, latest)
		org.Month = org.Month.plus(sumMonth(month))
	}
	org.Recent = topRecent(recent)
	org.Warn = oc.WarnBelow > 0 && org.TotalCents < int64(math.Round(oc.WarnBelow*100))
	res := fetched{org: org}
	if len(org.Accounts) == 1 {
		res.ledger = ledger
	}
	return res, nil
}

func (m Month) plus(o Month) Month {
	return Month{InCents: m.InCents + o.InCents, OutCents: m.OutCents + o.OutCents}
}

func sumMonth(txs []transaction) Month {
	var m Month
	for _, tx := range txs {
		if !tx.completed() {
			continue
		}
		switch tx.Side {
		case "credit":
			m.InCents += tx.AmountCents
		case "debit":
			m.OutCents += tx.AmountCents
		}
	}
	return m
}

func topRecent(txs []transaction) []Transaction {
	txs = slices.DeleteFunc(txs, func(tx transaction) bool { return !tx.completed() })
	slices.SortStableFunc(txs, func(a, b transaction) int { return b.at().Compare(a.at()) })
	txs = txs[:min(len(txs), recentCount)]
	out := make([]Transaction, 0, len(txs))
	for _, tx := range txs {
		out = append(out, Transaction{
			Label:        tx.Label,
			Counterparty: tx.CleanCounterpartyName,
			AmountCents:  tx.AmountCents,
			Side:         tx.Side,
			Type:         tx.OperationType,
			At:           tx.at(),
		})
	}
	return out
}

func monthStart(now time.Time, loc *time.Location) time.Time {
	n := now.In(loc)
	return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
}
