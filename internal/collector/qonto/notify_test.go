package qonto

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/kolapsis/pi-dashboard/internal/collector"
)

var _ collector.Notifier = (*Collector)(nil)

func TestNotify(t *testing.T) {
	at := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	in := Transaction{Label: "Stripe Payments UK Ltd", AmountCents: 44700, Side: "credit", At: at}
	out := Transaction{Label: "OVH SAS", AmountCents: 2398, Side: "debit", At: at.Add(-time.Hour)}
	prev := Data{Orgs: []Org{{Name: "kOlapsis SAS", Recent: []Transaction{in, out}}}}

	assert.Nil(t, Notify(prev, prev))
	newIn := Transaction{Label: "VIR SEPA", Counterparty: "ACME", AmountCents: 120000, Side: "credit", At: at.Add(time.Hour)}
	newOut := Transaction{Label: "URSSAF", AmountCents: 61200, Side: "debit", At: at.Add(2 * time.Hour)}
	assert.Equal(t, []string{"Reçu 1 200 € · ACME · kOlapsis SAS"},
		Notify(prev, Data{Orgs: []Org{{Name: "kOlapsis SAS", Recent: []Transaction{newOut, newIn, in}}}}))
	assert.Nil(t, Notify(prev, Data{Orgs: []Org{{Name: "EI", Recent: []Transaction{newIn}}}}), "unknown organisation is ignored")
	assert.Nil(t, Notify(prev, Data{Orgs: []Org{{Name: "kOlapsis SAS", Recent: []Transaction{}}}}))
	assert.Nil(t, Notify(nil, prev))
}
