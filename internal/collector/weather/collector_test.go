package weather

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/store"
)

var (
	_ collector.Collector  = (*Collector)(nil)
	_ collector.Summarizer = (*Collector)(nil)

	paris = time.FixedZone("Europe/Paris", 2*3600)
)

const minimalForecast = `{
  "current": {"temperature_2m": 14.2, "apparent_temperature": 12.9, "weather_code": 3, "is_day": 1},
  "daily": {
    "weather_code": [3, 61],
    "temperature_2m_max": [17.1, 16.0],
    "temperature_2m_min": [9.4, 11.2],
    "precipitation_probability_max": [20, 70],
    "sunrise": ["2026-10-07T08:07", "2026-10-08T08:08"],
    "sunset": ["2026-10-07T19:31", "2026-10-08T19:30"]
  }
}`

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

func respond(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func newCollector(t *testing.T, handler http.Handler) *Collector {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New(config.Weather{Label: "Bordeaux", Lat: 44.8378, Lon: -0.5792}, collector.Deps{
		HTTP:  srv.Client(),
		Clock: clock.NewFake(time.Date(2026, 10, 7, 12, 15, 0, 0, time.UTC)),
		Hist:  store.NewMem(),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Loc:   paris,
	})
	c.baseURL = srv.URL
	return c
}

func TestCollect(t *testing.T) {
	seen := make(chan *url.URL, 1)
	c := newCollector(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.URL
		respond(http.StatusOK, fixture(t, "forecast_day.json"))(w, r)
	}))

	got, err := c.Collect(context.Background())

	require.NoError(t, err)
	assert.Equal(t, Data{
		Label:    "Bordeaux",
		Temp:     14.2,
		Feels:    12.9,
		Code:     3,
		IsDay:    true,
		TMin:     9.4,
		TMax:     17.1,
		RainPct:  20,
		Sunrise:  time.Date(2026, 10, 7, 8, 7, 0, 0, paris),
		Sunset:   time.Date(2026, 10, 7, 19, 31, 0, 0, paris),
		Tomorrow: &Day{Code: 61, TMin: 11.2, TMax: 16, RainPct: 70},
	}, got)

	u := <-seen
	assert.Equal(t, "/v1/forecast", u.Path)
	assert.Equal(t, "latitude=44.8378&longitude=-0.5792"+
		"&current=temperature_2m,apparent_temperature,weather_code,is_day"+
		"&daily=weather_code,temperature_2m_max,temperature_2m_min,precipitation_probability_max,sunrise,sunset"+
		"&timezone=Europe%2FParis&forecast_days=2", u.RawQuery)
}

func TestCollectNight(t *testing.T) {
	c := newCollector(t, respond(http.StatusOK, fixture(t, "forecast_night.json")))

	got, err := c.Collect(context.Background())

	require.NoError(t, err)
	d := got.(Data)
	assert.False(t, d.IsDay)
	assert.InDelta(t, 19.2, d.Temp, 1e-9)
	assert.InDelta(t, 20.5, d.Feels, 1e-9)
	assert.Equal(t, 78, d.RainPct)
	assert.True(t, time.Date(2026, 10, 7, 8, 7, 0, 0, paris).Equal(d.Sunrise))
	assert.True(t, time.Date(2026, 10, 7, 19, 31, 0, 0, paris).Equal(d.Sunset))
	require.NotNil(t, d.Tomorrow)
	assert.Equal(t, Day{Code: 3, TMin: 12.2, TMax: 18.3, RainPct: 28}, *d.Tomorrow)
}

func TestCollectDerivedValues(t *testing.T) {
	tests := []struct {
		name string
		body string
		want func(t *testing.T, d Data)
	}{
		{
			name: "null rain probability is zero",
			body: strings.Replace(minimalForecast, "[20, 70]", "[null, null]", 1),
			want: func(t *testing.T, d Data) {
				assert.Equal(t, 0, d.RainPct)
				assert.Equal(t, 0, d.Tomorrow.RainPct)
			},
		},
		{
			name: "rain probability is rounded",
			body: strings.Replace(minimalForecast, "[20, 70]", "[19.6, 70.4]", 1),
			want: func(t *testing.T, d Data) {
				assert.Equal(t, 20, d.RainPct)
				assert.Equal(t, 70, d.Tomorrow.RainPct)
			},
		},
		{
			name: "single day has no tomorrow",
			body: `{"current": {"temperature_2m": 1, "apparent_temperature": -2.5, "weather_code": 71, "is_day": 0},
				"daily": {"weather_code": [71], "temperature_2m_max": [2], "temperature_2m_min": [-3],
				"precipitation_probability_max": [90], "sunrise": ["2026-12-21T08:40"], "sunset": ["2026-12-21T17:25"]}}`,
			want: func(t *testing.T, d Data) {
				assert.Nil(t, d.Tomorrow)
				assert.False(t, d.IsDay)
				assert.InDelta(t, -2.5, d.Feels, 1e-9)
				assert.InDelta(t, -3, d.TMin, 1e-9)
			},
		},
		{
			name: "tomorrow with a missing temperature is dropped",
			body: strings.Replace(minimalForecast, "[17.1, 16.0]", "[17.1, null]", 1),
			want: func(t *testing.T, d Data) { assert.Nil(t, d.Tomorrow) },
		},
		{
			name: "sunrise is read in the configured zone",
			body: minimalForecast,
			want: func(t *testing.T, d Data) {
				_, offset := d.Sunrise.Zone()
				assert.Equal(t, 2*3600, offset)
				assert.Equal(t, "06:07", d.Sunrise.UTC().Format("15:04"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCollector(t, respond(http.StatusOK, tt.body))

			got, err := c.Collect(context.Background())

			require.NoError(t, err)
			tt.want(t, got.(Data))
		})
	}
}

func TestCollectErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"missing current block", 200, `{"daily": {}}`, "incomplete forecast, missing current.temperature_2m, current.apparent_temperature, current.weather_code, current.is_day, daily.temperature_2m_min, daily.temperature_2m_max"},
		{"null current temperature", 200, strings.Replace(minimalForecast, `"temperature_2m": 14.2`, `"temperature_2m": null`, 1), "incomplete forecast, missing current.temperature_2m"},
		{"missing daily minimum", 200, strings.Replace(minimalForecast, "[9.4, 11.2]", "[]", 1), "incomplete forecast, missing daily.temperature_2m_min"},
		{"bad sunrise layout", 200, strings.Replace(minimalForecast, "2026-10-07T08:07", "07/10/2026 08:07", 1), "parse sunrise"},
		{"empty sunset", 200, strings.Replace(minimalForecast, `["2026-10-07T19:31", "2026-10-08T19:30"]`, "[]", 1), "sunset missing"},
		{"not json", 200, "<html>maintenance</html>", "decode"},
		{"api error", 400, `{"error": true, "reason": "Latitude must be in range of -90 to 90°. Given: 95.0."}`, "HTTP 400"},
		{"api error reason is kept", 400, `{"error": true, "reason": "Latitude must be in range of -90 to 90°. Given: 95.0."}`, "Latitude must be in range"},
		{"server error", 502, "bad gateway", "HTTP 502"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCollector(t, respond(tt.status, tt.body))

			got, err := c.Collect(context.Background())

			require.ErrorContains(t, err, tt.wantErr)
			assert.Nil(t, got)
		})
	}
}

func TestCollectTransportErrorDoesNotLeakQuery(t *testing.T) {
	c := newCollector(t, respond(http.StatusOK, minimalForecast))
	srv := httptest.NewServer(http.NotFoundHandler())
	c.baseURL = srv.URL
	srv.Close()

	_, err := c.Collect(context.Background())

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "latitude")
	assert.NotContains(t, err.Error(), "44.8378")
	assert.NotContains(t, err.Error(), "timezone")
}

func TestCollectCanceledContext(t *testing.T) {
	c := newCollector(t, respond(http.StatusOK, minimalForecast))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Collect(ctx)

	require.ErrorIs(t, err, context.Canceled)
}

func TestSummary(t *testing.T) {
	c := newCollector(t, respond(http.StatusOK, minimalForecast))

	got, err := c.Collect(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "Bordeaux 14.2° (code 3), 9/17°", c.Summary(got))
	assert.Empty(t, c.Summary("not weather data"))
}

func TestIdentity(t *testing.T) {
	c := New(config.Weather{Interval: 15 * time.Minute}, collector.Deps{})

	assert.Equal(t, "weather", c.Name())
	assert.Equal(t, 15*time.Minute, c.Interval())
}
