package umami

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
)

func isoRange(first string, n int) []string {
	start, err := time.Parse("2006-01-02", first)
	if err != nil {
		panic(err)
	}
	out := make([]string, n)
	for i := range out {
		out[i] = start.AddDate(0, 0, i).Format("2006-01-02")
	}
	return out
}

func pageviewsFrom(sessions map[string]int, x func(day time.Time) any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		var series []map[string]any
		for _, key := range slices.Sorted(maps.Keys(sessions)) {
			day, err := time.ParseInLocation("2006-01-02", key, paris)
			if err != nil {
				panic(err)
			}
			series = append(series, map[string]any{"x": x(day), "y": sessions[key]})
		}
		b, err := json.Marshal(map[string]any{"pageviews": series, "sessions": series})
		if err != nil {
			panic(err)
		}
		reply(w, http.StatusOK, b)
	}
}

func TestBarsAroundDSTChanges(t *testing.T) {
	autumn := map[string]int{
		"2026-10-12": 99, "2026-10-13": 10, "2026-10-14": 11, "2026-10-15": 12, "2026-10-16": 13, "2026-10-17": 4,
		"2026-10-18": 3, "2026-10-19": 14, "2026-10-21": 16, "2026-10-22": 17, "2026-10-23": 18, "2026-10-24": 5,
		"2026-10-26": 7, "2026-10-27": 55,
	}
	spring := map[string]int{}
	for d := 14; d <= 31; d++ {
		if d != 28 {
			spring[fmt.Sprintf("2027-03-%02d", d)] = d
		}
	}
	values := func(sessions map[string]int, days []string) []float64 {
		out := make([]float64, len(days))
		for i, d := range days {
			out[i] = float64(sessions[d])
		}
		return out
	}

	scenarios := []struct {
		name     string
		now      time.Time
		sessions map[string]int
		firstDay string
	}{
		{"day after the 25-hour day", time.Date(2026, 10, 26, 0, 30, 0, 0, paris), autumn, "2026-10-13"},
		{"evening of the 25-hour day", time.Date(2026, 10, 25, 23, 30, 0, 0, paris), autumn, "2026-10-12"},
		{"repeated hour of the 25-hour day", time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC).In(paris), autumn, "2026-10-12"},
		{"day after the 23-hour day", time.Date(2027, 3, 29, 0, 30, 0, 0, paris), spring, "2027-03-16"},
		{"evening of the 23-hour day", time.Date(2027, 3, 28, 23, 30, 0, 0, paris), spring, "2027-03-15"},
	}
	formats := []struct {
		name string
		x    func(day time.Time) any
	}{
		{"naive local time", func(d time.Time) any { return d.Format("2006-01-02 15:04:05") }},
		{"date only", func(d time.Time) any { return d.Format("2006-01-02") }},
		{"UTC midnight label", func(d time.Time) any { return d.Format("2006-01-02") + "T00:00:00.000Z" }},
		{"true instant, ISO", func(d time.Time) any { return d.UTC().Format("2006-01-02T15:04:05.000Z") }},
		{"true instant, unix ms", func(d time.Time) any { return d.UnixMilli() }},
	}

	for _, sc := range scenarios {
		for _, f := range formats {
			t.Run(sc.name+"/"+f.name, func(t *testing.T) {
				a := newAuth()
				routes := routesFor(t, a, "v2", "websites_data.json")
				routes["GET /api/websites/{id}/pageviews"] = a.guard(pageviewsFrom(sc.sessions, f.x))
				cfg := baseConfig()
				cfg.Sites = []config.UmamiSite{{ID: siteA}}
				e := newEnv(t, a, routes, cfg)
				e.c.deps.Clock = clock.NewFake(sc.now)

				d := e.collect(t)

				days := isoRange(sc.firstDay, 14)
				want := make([]collector.DayPoint, len(days))
				for i, v := range values(sc.sessions, days) {
					want[i] = collector.DayPoint{D: days[i], V: v}
				}
				assert.Equal(t, want, d.Bars14)
				require.Len(t, d.Sites, 1)
				assert.Equal(t, want, d.Sites[0].Bars14)

				first, err := time.ParseInLocation("2006-01-02", sc.firstDay, paris)
				require.NoError(t, err)
				for _, r := range e.srv.requests() {
					if r.path == "/api/websites/"+siteA+"/pageviews" {
						assert.Equal(t, fmt.Sprint(first.UnixMilli()), r.query.Get("startAt"))
						assert.Equal(t, fmt.Sprint(sc.now.UnixMilli()), r.query.Get("endAt"))
					}
				}
			})
		}
	}
}

func TestBarsMissingDaysAreZeroFilled(t *testing.T) {
	a := newAuth()
	routes := routesFor(t, a, "v3", "websites_data.json")
	routes["GET /api/websites/{id}/pageviews"] = a.guard(func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, []byte(`{"pageviews":[],"sessions":[{"x":"2026-10-07T00:00:00.000Z","y":4},{"x":"2026-09-24T00:00:00.000Z","y":9}]}`))
	})
	cfg := baseConfig()
	cfg.Sites = []config.UmamiSite{{ID: siteB}}
	e := newEnv(t, a, routes, cfg)

	d := e.collect(t)

	want := make([]float64, 14)
	want[0], want[13] = 9, 4
	assert.Equal(t, dayPoints(days14, want...), d.Bars14)
}

func TestBarsSumBucketsThatShareADay(t *testing.T) {
	a := newAuth()
	routes := routesFor(t, a, "v2", "websites_data.json")
	routes["GET /api/websites/{id}/pageviews"] = a.guard(func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, []byte(`{"pageviews":[],"sessions":[{"x":"2026-10-06 00:00:00","y":4},{"x":"2026-10-06 00:00:00","y":6},{"x":"not a date","y":50},{"y":70}]}`))
	})
	cfg := baseConfig()
	cfg.Sites = []config.UmamiSite{{ID: siteB}}
	e := newEnv(t, a, routes, cfg)

	d := e.collect(t)

	assert.Equal(t, collector.DayPoint{D: "2026-10-06", V: 10}, d.Bars14[12])
	var sum float64
	for _, b := range d.Bars14 {
		sum += b.V
	}
	assert.Equal(t, 10.0, sum, "buckets without a usable day are ignored")
}

func TestDaysNeverSkipOrRepeatADate(t *testing.T) {
	c := New(config.Umami{}, collector.Deps{Loc: paris})
	for _, span := range []struct{ from, to time.Time }{
		{time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)},
		{time.Date(2027, 3, 12, 0, 0, 0, 0, time.UTC), time.Date(2027, 4, 12, 0, 0, 0, 0, time.UTC)},
	} {
		for now := span.from; now.Before(span.to); now = now.Add(30 * time.Minute) {
			today, err := time.Parse("2006-01-02", now.In(paris).Format("2006-01-02"))
			require.NoError(t, err)
			want := isoRange(today.AddDate(0, 0, -(barDays-1)).Format("2006-01-02"), barDays)

			require.Equal(t, want, c.days(now), "at %s", now.In(paris).Format(time.RFC3339))
		}
	}
}
