package jsonpoll

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/httpx"
)

const deltaSpan = 7 * 24 * time.Hour

var formats = map[string]func(float64) string{
	"int":     formatInt,
	"float":   func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) },
	"percent": func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) + "%" },
}

type Collector struct {
	cfg  config.JSONPoll
	deps collector.Deps
}

func New(cfg config.JSONPoll, deps collector.Deps) *Collector {
	return &Collector{cfg: cfg, deps: deps}
}

func (c *Collector) Name() string { return c.cfg.Name }

func (c *Collector) Interval() time.Duration { return c.cfg.Interval }

func (c *Collector) Collect(ctx context.Context) (any, error) {
	if _, err := url.Parse(c.cfg.URL); err != nil {
		return nil, errors.New("invalid url")
	}
	var body json.RawMessage
	if err := httpx.GetJSON(ctx, c.deps.HTTP, c.cfg.URL, c.headers(), &body); err != nil {
		return nil, fmt.Errorf("fetch: %w", stripURL(err))
	}

	items := make([]Item, len(c.cfg.Items))
	var problems []string
	for i, it := range c.cfg.Items {
		item, err := render(it, gjson.GetBytes(body, it.Path))
		if err != nil {
			problems = append(problems, it.Key+": "+err.Error())
			continue
		}
		items[i] = item
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}

	now := c.deps.Clock.Now()
	for i, it := range c.cfg.Items {
		if it.History {
			c.track(ctx, &items[i], now)
		}
	}
	return Data{Name: c.cfg.Name, Items: items}, nil
}

func (c *Collector) headers() map[string]string {
	h := maps.Clone(c.cfg.Headers)
	if ba := c.cfg.BasicAuth; ba != nil {
		if h == nil {
			h = map[string]string{}
		}
		h["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(ba.User+":"+ba.Password))
	}
	return h
}

func (c *Collector) track(ctx context.Context, item *Item, now time.Time) {
	if delta, ok := collector.Delta(ctx, c.deps.Hist, c.Name(), item.Key, now, deltaSpan, item.Value); ok {
		item.Delta7 = &delta
	}
	collector.Record(ctx, c.deps.Hist, c.deps.Log, c.Name(), item.Key, now, item.Value)
}

func render(it config.JSONPollItem, r gjson.Result) (Item, error) {
	switch {
	case !r.Exists():
		return Item{}, fmt.Errorf("path %q not found", it.Path)
	case r.Type == gjson.Null:
		return Item{}, fmt.Errorf("path %q is null", it.Path)
	}
	text, err := format(r, it.Format)
	if err != nil {
		return Item{}, err
	}
	return Item{Key: it.Key, Label: cmp.Or(it.Label, it.Key), Value: value(r), Text: text}, nil
}

func format(r gjson.Result, name string) (string, error) {
	if name == "" {
		return r.String(), nil
	}
	f, ok := formats[name]
	if !ok {
		return "", fmt.Errorf("unknown format %q", name)
	}
	if v, ok := number(r); ok {
		return f(v), nil
	}
	return r.String(), nil
}

func value(r gjson.Result) float64 {
	if v, ok := number(r); ok {
		return v
	}
	if r.Type == gjson.True {
		return 1
	}
	return 0
}

func number(r gjson.Result) (float64, bool) {
	var v float64
	switch r.Type {
	case gjson.Number:
		v = r.Num
	case gjson.String:
		var err error
		if v, err = strconv.ParseFloat(r.Str, 64); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

func formatInt(v float64) string {
	r := math.Round(v)
	if r == 0 {
		return "0"
	}
	return strconv.FormatFloat(r, 'f', 0, 64)
}

func (c *Collector) Summary(data any) string {
	d, ok := data.(Data)
	if !ok {
		return ""
	}
	parts := make([]string, len(d.Items))
	for i, it := range d.Items {
		parts[i] = it.Key + "=" + it.Text
	}
	return strings.Join(parts, " ")
}

func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
