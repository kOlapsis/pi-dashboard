package stripe

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/kolapsis/pi-dashboard/internal/collector"
)

var _ collector.Notifier = (*Collector)(nil)

func TestNotify(t *testing.T) {
	at := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	pay := func(cents int64, label string, when time.Time) *Payment {
		return &Payment{AmountCents: cents, Currency: "eur", Label: label, At: when}
	}
	prev := Data{Accounts: []Account{
		{Name: "Maintenant", Last: pay(2900, "Maintenant Pro", at)},
		{Name: "Ackify"},
	}}

	assert.Nil(t, Notify(prev, prev))
	assert.Equal(t, []string{"Paiement 49 € · Maintenant · Maintenant Team"}, Notify(prev, Data{Accounts: []Account{
		{Name: "Maintenant", Last: pay(4900, "Maintenant Team", at.Add(time.Hour))},
		{Name: "Ackify"},
	}}))
	assert.Equal(t, []string{"Paiement 120 € · Ackify"}, Notify(prev, Data{Accounts: []Account{
		{Name: "Maintenant", Last: pay(2900, "Maintenant Pro", at)},
		{Name: "Ackify", Last: pay(12000, "", at.Add(time.Minute))},
	}}), "first payment of an account")
	assert.Nil(t, Notify(prev, Data{Accounts: []Account{
		{Name: "Maintenant", Last: pay(2900, "Maintenant Pro", at.Add(-time.Hour))},
		{Name: "New", Last: pay(100, "", at)},
	}}), "older payment and unknown account are ignored")
	assert.Nil(t, Notify(nil, prev))
}
