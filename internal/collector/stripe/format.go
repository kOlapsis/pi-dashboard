package stripe

import (
	"strconv"
	"strings"
)

func euros(cents int64) string {
	negative := cents < 0
	if negative {
		cents = -cents
	}
	whole := strconv.FormatInt((cents+50)/100, 10)
	var b strings.Builder
	if negative && whole != "0" {
		b.WriteString("−")
	}
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n > 1 {
		return many
	}
	return one
}

func clip(s string, limit int) string {
	if r := []rune(s); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
}
