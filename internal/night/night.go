package night

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Window struct {
	From string
	To   string
}

func parseHM(s string) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("want HH:MM, got %q", s)
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("want HH:MM, got %q", s)
	}
	return hh*60 + mm, nil
}

func (w Window) Validate() error {
	if _, err := parseHM(w.From); err != nil {
		return err
	}
	_, err := parseHM(w.To)
	return err
}

func (w Window) Active(now time.Time) bool {
	from, err1 := parseHM(w.From)
	to, err2 := parseHM(w.To)
	if err1 != nil || err2 != nil || from == to {
		return false
	}
	m := now.Hour()*60 + now.Minute()
	if from < to {
		return m >= from && m < to
	}
	return m >= from || m < to
}
