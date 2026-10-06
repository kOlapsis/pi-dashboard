package stripe

func (s subscription) mrr() (cents int64, priced, metered bool) {
	for _, it := range s.Items.Data {
		p := it.Price
		if !isEUR(p.Currency) || p.Recurring == nil {
			continue
		}
		if p.UnitAmount == nil || p.Recurring.UsageType == "metered" {
			metered = true
			continue
		}
		quantity := int64(1)
		if it.Quantity != nil {
			quantity = *it.Quantity
		}
		monthly, ok := monthlyCents(*p.UnitAmount*quantity, p.Recurring.Interval, p.Recurring.IntervalCount)
		if !ok {
			continue
		}
		cents += monthly
		priced = true
	}
	return cents, priced, metered
}

func monthlyCents(amount int64, interval string, count int64) (int64, bool) {
	count = max(count, 1)
	switch interval {
	case "month":
		return roundDiv(amount, count), true
	case "year":
		return roundDiv(amount, 12*count), true
	case "week":
		return roundDiv(amount*52, 12*count), true
	case "day":
		return roundDiv(amount*365, 12*count), true
	}
	return 0, false
}

func roundDiv(n, d int64) int64 { return (n + d/2) / d }
