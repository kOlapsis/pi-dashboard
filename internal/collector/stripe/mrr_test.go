package stripe

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func item(currency, unit, quantity, interval string, count int, usage string) string {
	return fmt.Sprintf(`{"quantity":%s,"price":{"currency":%q,"unit_amount":%s,"recurring":{"interval":%q,"interval_count":%d,"usage_type":%q}}}`,
		quantity, currency, unit, interval, count, usage)
}

func decodeSubscription(t *testing.T, items ...string) subscription {
	t.Helper()
	var s subscription
	raw := `{"id":"sub_1Q9xFAKE0000000000000S99","items":{"data":[` + strings.Join(items, ",") + `]}}`
	require.NoError(t, json.Unmarshal([]byte(raw), &s))
	return s
}

func TestSubscriptionMRR(t *testing.T) {
	tests := []struct {
		name    string
		items   []string
		cents   int64
		priced  bool
		metered bool
	}{
		{"monthly", []string{item("eur", "2900", "1", "month", 1, "licensed")}, 2900, true, false},
		{"yearly rounds up", []string{item("eur", "14900", "1", "year", 1, "licensed")}, 1242, true, false},
		{"yearly rounds down", []string{item("eur", "14500", "1", "year", 1, "licensed")}, 1208, true, false},
		{"every two years", []string{item("eur", "24000", "1", "year", 2, "licensed")}, 1000, true, false},
		{"weekly", []string{item("eur", "700", "1", "week", 1, "licensed")}, 3033, true, false},
		{"every two weeks", []string{item("eur", "1000", "1", "week", 2, "licensed")}, 2167, true, false},
		{"daily", []string{item("eur", "100", "1", "day", 1, "licensed")}, 3042, true, false},
		{"every three months", []string{item("eur", "8700", "1", "month", 3, "licensed")}, 2900, true, false},
		{"missing interval count", []string{item("eur", "2900", "1", "month", 0, "licensed")}, 2900, true, false},
		{"quantity 2", []string{item("eur", "4900", "2", "month", 1, "licensed")}, 9800, true, false},
		{"quantity 3 yearly", []string{item("eur", "14900", "3", "year", 1, "licensed")}, 3725, true, false},
		{"quantity 0", []string{item("eur", "4900", "0", "month", 1, "licensed")}, 0, true, false},
		{"missing quantity counts as 1", []string{item("eur", "2900", "null", "month", 1, "licensed")}, 2900, true, false},
		{"metered", []string{item("eur", "2", "null", "month", 1, "metered")}, 0, false, true},
		{"tiered", []string{item("eur", "null", "1", "month", 1, "licensed")}, 0, false, true},
		{"usd skipped", []string{item("usd", "5000", "1", "month", 1, "licensed")}, 0, false, false},
		{"usd metered skipped", []string{item("usd", "2", "null", "month", 1, "metered")}, 0, false, false},
		{"upper-case currency", []string{item("EUR", "2900", "1", "month", 1, "licensed")}, 2900, true, false},
		{"licensed and metered", []string{item("eur", "2900", "1", "month", 1, "licensed"), item("eur", "2", "null", "month", 1, "metered")}, 2900, true, true},
		{"two licensed items", []string{item("eur", "2900", "1", "month", 1, "licensed"), item("eur", "1000", "1", "year", 1, "licensed")}, 2983, true, false},
		{"unknown interval", []string{item("eur", "2900", "1", "decade", 1, "licensed")}, 0, false, false},
		{"one-time price", []string{`{"quantity":1,"price":{"currency":"eur","unit_amount":500,"recurring":null}}`}, 0, false, false},
		{"no items", nil, 0, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cents, priced, metered := decodeSubscription(t, tt.items...).mrr()
			assert.Equal(t, tt.cents, cents)
			assert.Equal(t, tt.priced, priced, "priced")
			assert.Equal(t, tt.metered, metered, "metered")
		})
	}
}

func TestRoundDiv(t *testing.T) {
	tests := []struct{ n, d, want int64 }{
		{10, 4, 3},
		{9, 4, 2},
		{11, 4, 3},
		{0, 7, 0},
		{12, 12, 1},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, roundDiv(tt.n, tt.d), "%d/%d", tt.n, tt.d)
	}
}
