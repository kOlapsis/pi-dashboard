package umami

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const dayLayout = "2006-01-02"

type metric int

func (m *metric) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '{' {
		var o struct {
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(b, &o); err != nil {
			return err
		}
		b = bytes.TrimSpace(o.Value)
	}
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*m = 0
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("metric %q is not a number", s)
	}
	*m = metric(math.Round(f))
	return nil
}

type stats struct {
	Visits    metric `json:"visits"`
	Pageviews metric `json:"pageviews"`
}

type bucket struct {
	X json.RawMessage `json:"x"`
	Y metric          `json:"y"`
}

type pageviewSeries struct {
	Sessions []bucket `json:"sessions"`
}

type website struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Domain string `json:"domain"`
}

type websiteList []website

func (l *websiteList) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '[' {
		return json.Unmarshal(b, (*[]website)(l))
	}
	var o struct {
		Data []website `json:"data"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	*l = o.Data
	return nil
}

type activeItem struct {
	Visitors *metric `json:"visitors"`
	X        *metric `json:"x"`
}

func (it activeItem) value() int {
	switch {
	case it.Visitors != nil:
		return int(*it.Visitors)
	case it.X != nil:
		return int(*it.X)
	}
	return 0
}

type active int

func (a *active) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	*a = 0
	if len(b) == 0 {
		return nil
	}
	switch b[0] {
	case '[':
		var items []activeItem
		if err := json.Unmarshal(b, &items); err != nil {
			return err
		}
		if len(items) > 0 {
			*a = active(items[0].value())
		}
	case '{':
		var it activeItem
		if err := json.Unmarshal(b, &it); err != nil {
			return err
		}
		*a = active(it.value())
	default:
		var m metric
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		*a = active(m)
	}
	return nil
}

var naiveLayouts = []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", dayLayout}

func (b bucket) day(loc *time.Location) (string, bool) {
	var s string
	if err := json.Unmarshal(b.X, &s); err == nil {
		return stringDay(strings.TrimSpace(s), loc)
	}
	var ms float64
	if err := json.Unmarshal(b.X, &ms); err == nil {
		return instantDay(time.UnixMilli(int64(ms)), loc), true
	}
	return "", false
}

func stringDay(s string, loc *time.Location) (string, bool) {
	for _, layout := range naiveLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format(dayLayout), true
		}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return instantDay(t, loc), true
	}
	return "", false
}

// Umami labels local day buckets as UTC midnight, so only the other instants are converted to local time.
func instantDay(t time.Time, loc *time.Location) string {
	if u := t.UTC(); u.Hour() == 0 && u.Minute() == 0 && u.Second() == 0 {
		return u.Format(dayLayout)
	}
	return t.In(loc).Format(dayLayout)
}

func pct(cur, prev int) *float64 {
	if prev == 0 {
		return nil
	}
	v := float64(int(math.Round(float64(cur-prev)*1000/float64(prev)))) / 10
	return &v
}
