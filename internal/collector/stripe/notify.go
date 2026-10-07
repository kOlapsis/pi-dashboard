package stripe

func (c *Collector) Notify(prev, cur any) []string { return Notify(prev, cur) }

// Notify reports one label per account whose last payment is newer than in prev.
func Notify(prev, cur any) []string {
	p, ok1 := asData(prev)
	n, ok2 := asData(cur)
	if !ok1 || !ok2 {
		return nil
	}
	last := make(map[string]*Payment, len(p.Accounts))
	for _, a := range p.Accounts {
		last[a.Name] = a.Last
	}
	var out []string
	for _, a := range n.Accounts {
		if a.Last == nil {
			continue
		}
		if before, known := last[a.Name]; !known || (before != nil && !a.Last.At.After(before.At)) {
			continue
		}
		label := "Paiement " + euros(a.Last.AmountCents) + " € · " + a.Name
		if a.Last.Label != "" {
			label += " · " + clip(a.Last.Label, 40)
		}
		out = append(out, label)
	}
	return out
}

func asData(v any) (Data, bool) {
	switch d := v.(type) {
	case Data:
		return d, true
	case *Data:
		if d != nil {
			return *d, true
		}
	}
	return Data{}, false
}
