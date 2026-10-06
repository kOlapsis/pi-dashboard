package health

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
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
	_ collector.Timeouter  = (*Collector)(nil)
)

var t0 = time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC)

func newCollector(t *testing.T, client *http.Client, threshold int, sites ...config.HealthSite) *Collector {
	t.Helper()
	return New(config.Health{FailThreshold: threshold, Sites: sites}, collector.Deps{
		HTTP:  client,
		Clock: clock.NewFake(t0),
		Hist:  store.NewMem(),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Loc:   time.UTC,
	})
}

func collect(t *testing.T, c *Collector) Data {
	t.Helper()
	got, err := c.Collect(context.Background())
	require.NoError(t, err)
	return got.(Data)
}

func site(name, url string) config.HealthSite { return config.HealthSite{Name: name, URL: url} }

func serve(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func closedServerURL() string {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }
}

func TestCollectReportsStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", status(http.StatusOK))
	mux.HandleFunc("/created", status(http.StatusCreated))
	mux.HandleFunc("/not-modified", status(http.StatusNotModified))
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ok", http.StatusFound) })
	mux.HandleFunc("/missing", status(http.StatusNotFound))
	mux.HandleFunc("/broken", status(http.StatusInternalServerError))
	srv := serve(t, mux)

	tests := []struct {
		path       string
		wantUp     bool
		wantStatus int
		wantErr    string
	}{
		{"/ok", true, 200, ""},
		{"/created", true, 201, ""},
		{"/not-modified", true, 304, ""},
		{"/moved", true, 200, ""},
		{"/missing", false, 404, "HTTP 404"},
		{"/broken", false, 500, "HTTP 500"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			c := newCollector(t, srv.Client(), 1, site("s", srv.URL+tt.path))

			got := collect(t, c)

			require.Len(t, got.Sites, 1)
			s := got.Sites[0]
			assert.Equal(t, tt.wantUp, s.Up)
			assert.Equal(t, tt.wantStatus, s.Status)
			assert.Equal(t, tt.wantErr, s.Error)
			assert.Nil(t, s.CertDays)
		})
	}
}

func TestCollectCountsAndKeepsOrder(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", status(http.StatusOK))
	mux.HandleFunc("/ko", status(http.StatusBadGateway))
	srv := serve(t, mux)
	c := newCollector(t, srv.Client(), 1,
		site("zeta", srv.URL+"/ok"),
		site("alpha", srv.URL+"/ko"),
		site("mid", srv.URL+"/ok"),
	)

	got := collect(t, c)

	assert.Equal(t, 3, got.Total)
	assert.Equal(t, 2, got.Up)
	names := make([]string, len(got.Sites))
	for i, s := range got.Sites {
		names[i] = s.Name
	}
	assert.Equal(t, []string{"zeta", "alpha", "mid"}, names)
}

func TestRedirectLimit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/hop/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		if n == 0 {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/hop/%d", n-1), http.StatusFound)
	})
	srv := serve(t, mux)

	tests := []struct {
		path    string
		wantUp  bool
		wantErr string
	}{
		{"/hop/3", true, ""},
		{"/hop/4", false, "stopped after 3 redirects"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			c := newCollector(t, srv.Client(), 1, site("s", srv.URL+tt.path))

			s := collect(t, c).Sites[0]

			assert.Equal(t, tt.wantUp, s.Up)
			assert.Equal(t, tt.wantErr, s.Error)
		})
	}
}

func TestFailureThreshold(t *testing.T) {
	var code atomic.Int32
	code.Store(http.StatusOK)
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(code.Load())) }))
	c := newCollector(t, srv.Client(), 3, site("s", srv.URL))

	steps := []struct {
		name    string
		code    int
		wantUp  bool
		wantErr string
	}{
		{"healthy", 200, true, ""},
		{"first failure keeps up", 503, true, "HTTP 503"},
		{"second failure keeps up", 503, true, "HTTP 503"},
		{"recovery resets the counter", 200, true, ""},
		{"first failure again", 503, true, "HTTP 503"},
		{"second failure again", 503, true, "HTTP 503"},
		{"third failure goes down", 503, false, "HTTP 503"},
		{"still down", 503, false, "HTTP 503"},
		{"recovery is immediate", 200, true, ""},
	}
	for _, step := range steps {
		code.Store(int32(step.code))

		got := collect(t, c)

		s := got.Sites[0]
		assert.Equal(t, step.wantUp, s.Up, step.name)
		assert.Equal(t, step.wantErr, s.Error, step.name)
		assert.Equal(t, step.code, s.Status, step.name)
		wantCount := 0
		if step.wantUp {
			wantCount = 1
		}
		assert.Equal(t, wantCount, got.Up, step.name)
	}
}

func TestFirstCheckFailure(t *testing.T) {
	url := closedServerURL()
	tests := []struct {
		name      string
		threshold int
		wantUp    bool
	}{
		{"default threshold gives the benefit of the doubt", 2, true},
		{"threshold of one reports immediately", 1, false},
		{"unset threshold behaves like one", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCollector(t, http.DefaultClient, tt.threshold, site("s", url))

			s := collect(t, c).Sites[0]

			assert.Equal(t, tt.wantUp, s.Up)
			assert.NotEmpty(t, s.Error)
		})
	}
}

func TestConnectionRefused(t *testing.T) {
	c := newCollector(t, http.DefaultClient, 1, site("s", closedServerURL()+"/?token=hunter2"))

	s := collect(t, c).Sites[0]

	assert.False(t, s.Up)
	assert.Zero(t, s.Status)
	assert.Zero(t, s.LatencyMs)
	assert.Nil(t, s.CertDays)
	assert.Contains(t, s.Error, "refused")
	assert.NotContains(t, s.Error, "hunter2")
	assert.NotContains(t, s.URL, "hunter2")
}

func TestTimeout(t *testing.T) {
	srv := serve(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	c := newCollector(t, srv.Client(), 1, site("s", srv.URL))
	c.timeout = 100 * time.Millisecond

	s := collect(t, c).Sites[0]

	assert.False(t, s.Up)
	assert.Equal(t, "timeout", s.Error)
	assert.Zero(t, s.Status)
}

func TestInvalidURL(t *testing.T) {
	c := newCollector(t, http.DefaultClient, 1, site("s", "http://[::1"))

	s := collect(t, c).Sites[0]

	assert.False(t, s.Up)
	assert.Equal(t, "invalid url", s.Error)
}

func TestLatencyStopsAtHeaders(t *testing.T) {
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(w, "late body")
	}))
	c := newCollector(t, srv.Client(), 1, site("s", srv.URL))
	c.deps.Clock = clock.Real{}

	start := time.Now()
	s := collect(t, c).Sites[0]

	assert.GreaterOrEqual(t, time.Since(start), 400*time.Millisecond)
	assert.Less(t, s.LatencyMs, 300)
	assert.True(t, s.Up)
}

func TestLatencyIncludesServerThinkTime(t *testing.T) {
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(120 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	c := newCollector(t, srv.Client(), 1, site("s", srv.URL))
	c.deps.Clock = clock.Real{}

	s := collect(t, c).Sites[0]

	assert.GreaterOrEqual(t, s.LatencyMs, 120)
	assert.Less(t, s.LatencyMs, 2000)
}

func TestBodyIsCapped(t *testing.T) {
	served := make(chan int64, 1)
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 16<<10)
		var n int64
		for r.Context().Err() == nil {
			m, err := w.Write(chunk)
			n += int64(m)
			if err != nil {
				break
			}
		}
		served <- n
	}))
	c := newCollector(t, srv.Client(), 1, site("s", srv.URL))
	c.timeout = 5 * time.Second

	start := time.Now()
	s := collect(t, c).Sites[0]

	assert.Less(t, time.Since(start), 2*time.Second)
	assert.True(t, s.Up)
	assert.Equal(t, 200, s.Status)
	<-served
}

func TestRequestHeaders(t *testing.T) {
	seen := make(chan http.Header, 1)
	method := make(chan string, 1)
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		method <- r.Method
	}))
	c := newCollector(t, srv.Client(), 1, site("s", srv.URL))

	collect(t, c)

	h := <-seen
	assert.Equal(t, "text/html,*/*", h.Get("Accept"))
	assert.Contains(t, h.Get("User-Agent"), "pi-dashboard")
	assert.Equal(t, http.MethodGet, <-method)
}

func TestConcurrencyIsBounded(t *testing.T) {
	var (
		mu             sync.Mutex
		inflight, peak int
		once           sync.Once
	)
	full := make(chan struct{})
	srv := serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		inflight++
		peak = max(peak, inflight)
		saturated := inflight >= concurrency
		mu.Unlock()
		if saturated {
			once.Do(func() { close(full) })
		}
		select {
		case <-full:
		case <-time.After(5 * time.Second):
		}
		time.Sleep(60 * time.Millisecond)
		mu.Lock()
		inflight--
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	sites := make([]config.HealthSite, 12)
	for i := range sites {
		sites[i] = site(fmt.Sprintf("s%d", i), srv.URL)
	}
	c := newCollector(t, srv.Client(), 1, sites...)

	got := collect(t, c)

	assert.Equal(t, 12, got.Up)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, concurrency, peak)
}

func TestCertDaysOverTLS(t *testing.T) {
	srv := httptest.NewTLSServer(status(http.StatusOK))
	t.Cleanup(srv.Close)
	c := newCollector(t, srv.Client(), 1, site("s", srv.URL))

	s := collect(t, c).Sites[0]

	require.NotNil(t, s.CertDays)
	want := int(srv.Certificate().NotAfter.Sub(t0).Hours() / 24)
	assert.Equal(t, want, *s.CertDays)
	assert.Greater(t, *s.CertDays, 365)
	assert.True(t, s.Up)
}

func TestDaysUntil(t *testing.T) {
	tests := []struct {
		name     string
		notAfter time.Time
		want     int
	}{
		{"two months", t0.Add(61*24*time.Hour + 18*time.Hour), 61},
		{"exactly one day", t0.Add(24 * time.Hour), 1},
		{"less than a day", t0.Add(23*time.Hour + 59*time.Minute), 0},
		{"now", t0, 0},
		{"expired an hour ago", t0.Add(-time.Hour), -1},
		{"expired a day and an hour ago", t0.Add(-25 * time.Hour), -2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, daysUntil(tt.notAfter, t0))
		})
	}
}

func TestURLInDataIsRedacted(t *testing.T) {
	srv := serve(t, status(http.StatusOK))
	c := newCollector(t, srv.Client(), 1, site("s", srv.URL+"/health?token=hunter2"))

	s := collect(t, c).Sites[0]

	assert.True(t, s.Up)
	assert.NotContains(t, s.URL, "hunter2")
	assert.Contains(t, s.URL, "/health")
}

func TestNoSites(t *testing.T) {
	c := newCollector(t, http.DefaultClient, 2)

	got, err := c.Collect(context.Background())

	require.ErrorContains(t, err, "no sites")
	assert.Nil(t, got)
}

func TestCanceledCollectDoesNotCountAsFailure(t *testing.T) {
	srv := serve(t, status(http.StatusOK))
	c := newCollector(t, srv.Client(), 2, site("s", srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := c.Collect(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, got)
	assert.Zero(t, c.state[0].failures)
	assert.True(t, collect(t, c).Sites[0].Up)
}

func TestTimeoutScalesWithSites(t *testing.T) {
	tests := []struct {
		sites int
		want  time.Duration
	}{
		{1, 15 * time.Second},
		{5, 15 * time.Second},
		{6, 25 * time.Second},
		{11, 35 * time.Second},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.sites), func(t *testing.T) {
			c := newCollector(t, http.DefaultClient, 2, make([]config.HealthSite, tt.sites)...)

			assert.Equal(t, tt.want, c.Timeout())
		})
	}
}

func TestSummary(t *testing.T) {
	c := newCollector(t, http.DefaultClient, 2)
	tests := []struct {
		name string
		data any
		want string
	}{
		{
			name: "all up",
			data: Data{Up: 3, Total: 3, Sites: []Site{
				{Name: "a", Up: true, LatencyMs: 120},
				{Name: "demo", Up: true, LatencyMs: 290},
				{Name: "c", Up: true, LatencyMs: 80},
			}},
			want: "3/3 up, slowest demo 290 ms",
		},
		{
			name: "a down site is listed and never the slowest",
			data: Data{Up: 2, Total: 3, Sites: []Site{
				{Name: "a", Up: true, LatencyMs: 120},
				{Name: "umami", Up: false, LatencyMs: 9000, Error: "HTTP 503"},
				{Name: "c", Up: true, LatencyMs: 80},
			}},
			want: "2/3 up, slowest a 120 ms, down: umami (HTTP 503)",
		},
		{
			name: "a site still under the failure threshold is flagged",
			data: Data{Up: 2, Total: 2, Sites: []Site{
				{Name: "a", Up: true, LatencyMs: 120},
				{Name: "b", Up: true, Error: "timeout"},
			}},
			want: "2/2 up, slowest a 120 ms, failing: b (timeout)",
		},
		{
			name: "everything down",
			data: Data{Total: 2, Sites: []Site{{Name: "a", Error: "timeout"}, {Name: "b", Error: "HTTP 500"}}},
			want: "0/2 up, down: a (timeout), b (HTTP 500)",
		},
		{name: "foreign data", data: "nope", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, c.Summary(tt.data))
		})
	}
}

func TestIdentity(t *testing.T) {
	c := New(config.Health{Interval: time.Minute}, collector.Deps{})

	assert.Equal(t, "health", c.Name())
	assert.Equal(t, time.Minute, c.Interval())
}

func TestNilLoggerIsTolerated(t *testing.T) {
	c := New(config.Health{FailThreshold: 1, Sites: []config.HealthSite{site("s", closedServerURL())}}, collector.Deps{
		HTTP:  http.DefaultClient,
		Clock: clock.NewFake(t0),
	})

	got := collect(t, c)

	assert.False(t, got.Sites[0].Up)
}
