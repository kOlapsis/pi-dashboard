package health

func (c *Collector) Notify(prev, cur any) []string { return Notify(prev, cur) }

// Notify reports the sites that were up in prev and are down in cur.
func Notify(prev, cur any) []string {
	p, ok1 := asData(prev)
	n, ok2 := asData(cur)
	if !ok1 || !ok2 {
		return nil
	}
	wasUp := make(map[string]bool, len(p.Sites))
	for _, s := range p.Sites {
		wasUp[s.Name] = s.Up
	}
	var out []string
	for _, s := range n.Sites {
		if !s.Up && wasUp[s.Name] {
			out = append(out, s.Name+" est down")
		}
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
