package weather

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/httpx"
)

const (
	defaultBaseURL = "https://api.open-meteo.com"
	forecastParams = "current=temperature_2m,apparent_temperature,weather_code,is_day" +
		"&daily=weather_code,temperature_2m_max,temperature_2m_min,precipitation_probability_max,sunrise,sunset"
	localTime = "2006-01-02T15:04"
)

type Collector struct {
	cfg     config.Weather
	deps    collector.Deps
	baseURL string
}

func New(cfg config.Weather, deps collector.Deps) *Collector {
	return &Collector{cfg: cfg, deps: deps, baseURL: defaultBaseURL}
}

func (c *Collector) Name() string { return "weather" }

func (c *Collector) Interval() time.Duration { return c.cfg.Interval }

type forecast struct {
	Current struct {
		Temp  *float64 `json:"temperature_2m"`
		Feels *float64 `json:"apparent_temperature"`
		Code  *float64 `json:"weather_code"`
		IsDay *float64 `json:"is_day"`
	} `json:"current"`
	Daily struct {
		Code    []*float64 `json:"weather_code"`
		TMax    []*float64 `json:"temperature_2m_max"`
		TMin    []*float64 `json:"temperature_2m_min"`
		Rain    []*float64 `json:"precipitation_probability_max"`
		Sunrise []string   `json:"sunrise"`
		Sunset  []string   `json:"sunset"`
	} `json:"daily"`
}

func (c *Collector) Collect(ctx context.Context) (any, error) {
	var f forecast
	if err := httpx.GetJSON(ctx, c.deps.HTTP, c.forecastURL(), nil, &f); err != nil {
		return nil, fmt.Errorf("fetch forecast: %w", stripURL(err))
	}
	d, err := c.build(f)
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (c *Collector) forecastURL() string {
	return fmt.Sprintf("%s/v1/forecast?latitude=%s&longitude=%s&%s&timezone=%s&forecast_days=2",
		c.baseURL,
		strconv.FormatFloat(c.cfg.Lat, 'f', -1, 64),
		strconv.FormatFloat(c.cfg.Lon, 'f', -1, 64),
		forecastParams,
		url.QueryEscape(c.deps.Loc.String()))
}

func (c *Collector) build(f forecast) (Data, error) {
	cur, day := f.Current, f.Daily
	tmin, tmax := at(day.TMin, 0), at(day.TMax, 0)
	required := []struct {
		field string
		value *float64
	}{
		{"current.temperature_2m", cur.Temp},
		{"current.apparent_temperature", cur.Feels},
		{"current.weather_code", cur.Code},
		{"current.is_day", cur.IsDay},
		{"daily.temperature_2m_min", tmin},
		{"daily.temperature_2m_max", tmax},
	}
	var missing []string
	for _, r := range required {
		if r.value == nil {
			missing = append(missing, r.field)
		}
	}
	if len(missing) > 0 {
		return Data{}, fmt.Errorf("incomplete forecast, missing %s", strings.Join(missing, ", "))
	}
	sunrise, err := c.parseLocal("sunrise", day.Sunrise)
	if err != nil {
		return Data{}, err
	}
	sunset, err := c.parseLocal("sunset", day.Sunset)
	if err != nil {
		return Data{}, err
	}
	d := Data{
		Label:   c.cfg.Label,
		Temp:    *cur.Temp,
		Feels:   *cur.Feels,
		Code:    round(*cur.Code),
		IsDay:   *cur.IsDay == 1,
		TMin:    *tmin,
		TMax:    *tmax,
		RainPct: rain(day.Rain, 0),
		Sunrise: sunrise,
		Sunset:  sunset,
	}
	if code, lo, hi := at(day.Code, 1), at(day.TMin, 1), at(day.TMax, 1); code != nil && lo != nil && hi != nil {
		d.Tomorrow = &Day{Code: round(*code), TMin: *lo, TMax: *hi, RainPct: rain(day.Rain, 1)}
	}
	return d, nil
}

func (c *Collector) parseLocal(field string, vals []string) (time.Time, error) {
	if len(vals) == 0 {
		return time.Time{}, fmt.Errorf("%s missing from forecast", field)
	}
	t, err := time.ParseInLocation(localTime, vals[0], c.deps.Loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse %s: %w", field, err)
	}
	return t, nil
}

func (c *Collector) Summary(data any) string {
	d, ok := data.(Data)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s %.1f° (code %d), %.0f/%.0f°", d.Label, d.Temp, d.Code, d.TMin, d.TMax)
}

func at(s []*float64, i int) *float64 {
	if i < len(s) {
		return s[i]
	}
	return nil
}

func round(v float64) int { return int(math.Round(v)) }

func rain(s []*float64, i int) int {
	if p := at(s, i); p != nil {
		return round(*p)
	}
	return 0
}

func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
