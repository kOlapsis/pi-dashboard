package qonto

func (c *Collector) Notify(prev, cur any) []string { return Notify(prev, cur) }

// Notify reports the credit transactions present in cur and absent from prev, per organisation.
func Notify(prev, cur any) []string {
	p, ok1 := asData(prev)
	n, ok2 := asData(cur)
	if !ok1 || !ok2 {
		return nil
	}
	seen := map[string]map[string]bool{}
	for _, o := range p.Orgs {
		seen[o.Name] = map[string]bool{}
		for _, t := range o.Recent {
			seen[o.Name][txKey(t)] = true
		}
	}
	var out []string
	for _, o := range n.Orgs {
		known, ok := seen[o.Name]
		if !ok {
			continue
		}
		for _, t := range o.Recent {
			if t.Side != "credit" || known[txKey(t)] {
				continue
			}
			who := t.Counterparty
			if who == "" {
				who = t.Label
			}
			out = append(out, "Reçu "+euros(t.AmountCents)+" € · "+clip(who, 40)+" · "+o.Name)
		}
	}
	return out
}

func txKey(t Transaction) string {
	return t.Label + "\x00" + t.Side + "\x00" + euros(t.AmountCents) + "\x00" + t.At.UTC().Format("2006-01-02T15:04:05")
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
