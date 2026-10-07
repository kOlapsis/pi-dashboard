package mail

import "fmt"

const maxListed = 3

func (c *Collector) Notify(prev, cur any) []string { return Notify(prev, cur) }

// Notify lists the unread messages present in cur and absent from prev.
func Notify(prev, cur any) []string {
	p, ok1 := asData(prev)
	n, ok2 := asData(cur)
	if !ok1 || !ok2 {
		return nil
	}
	seen := make(map[string]bool, len(p.Items))
	for _, it := range p.Items {
		seen[itemKey(it)] = true
	}
	var fresh []Item
	for _, it := range n.Items {
		if !seen[itemKey(it)] {
			fresh = append(fresh, it)
		}
	}
	if len(fresh) == 0 && n.Unseen > p.Unseen {
		return []string{fmt.Sprintf("%d %s", n.Unseen-p.Unseen, pluralMail(n.Unseen-p.Unseen))}
	}
	if len(fresh) > maxListed {
		return []string{fmt.Sprintf("%d nouveaux mails", len(fresh))}
	}
	if len(fresh) == 0 {
		return nil
	}
	out := make([]string, 0, len(fresh))
	for _, it := range fresh {
		label := "Mail de " + it.From
		if it.Subject != "" {
			label += " · " + clip(it.Subject, 60)
		}
		out = append(out, label)
	}
	return out
}

func itemKey(it Item) string {
	return it.From + "\x00" + it.Subject + "\x00" + it.At.UTC().Format("2006-01-02T15:04:05")
}

func pluralMail(n int) string {
	if n > 1 {
		return "nouveaux mails"
	}
	return "nouveau mail"
}

func clip(s string, limit int) string {
	if r := []rune(s); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
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
