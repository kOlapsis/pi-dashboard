package maintenant

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

const (
	statusPath     = "/status/api"
	alertsPath     = "/api/v1/alerts/active"
	hostsPath      = "/api/v1/resources/hosts"
	containersPath = "/api/v1/containers"
)

type route struct {
	status int
	body   string
}

type request struct {
	path   string
	header http.Header
}

type fake struct {
	srv *httptest.Server

	mu     sync.Mutex
	routes map[string]route
	seen   []request
}

func (f *fake) set(path string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[path] = route{status, body}
}

func (f *fake) requests() []request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]request(nil), f.seen...)
}

func (f *fake) paths() []string {
	var out []string
	for _, r := range f.requests() {
		out = append(out, r.path)
	}
	return out
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, request{r.URL.Path, r.Header.Clone()})
	rt, ok := f.routes[r.URL.Path]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rt.status)
	_, _ = io.WriteString(w, rt.body)
}

func newFake(t *testing.T, routes map[string]string) *fake {
	t.Helper()
	f := &fake{routes: map[string]route{}}
	for p, body := range routes {
		f.routes[p] = route{http.StatusOK, body}
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

func statusFake(t *testing.T, fixtureName string) *fake {
	return newFake(t, map[string]string{statusPath: fixture(t, fixtureName)})
}

func apiFake(t *testing.T, alertsFixture string) *fake {
	return newFake(t, map[string]string{
		alertsPath:     fixture(t, alertsFixture),
		hostsPath:      fixture(t, "hosts.json"),
		containersPath: fixture(t, "containers.json"),
	})
}

func newCollector(t *testing.T, instances ...config.MaintenantInstance) *Collector {
	t.Helper()
	return New(config.Maintenant{Instances: instances}, collector.Deps{
		HTTP:  http.DefaultClient,
		Clock: clock.NewFake(time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC)),
		Hist:  store.NewMem(),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Loc:   time.UTC,
	})
}

func instance(name string, f *fake, mode string) config.MaintenantInstance {
	return config.MaintenantInstance{Name: name, URL: f.srv.URL, Mode: mode}
}

func collect(t *testing.T, c *Collector) Data {
	t.Helper()
	got, err := c.Collect(context.Background())
	require.NoError(t, err)
	return got.(Data)
}

func ptr[T any](v T) *T { return &v }

func TestStatusMode(t *testing.T) {
	f := statusFake(t, "status_operational.json")
	c := newCollector(t, instance("prod", f, "status"))

	d := collect(t, c)

	assert.Equal(t, Data{Instances: []Instance{{
		Name:            "prod",
		Status:          "operational",
		Message:         "All systems operational",
		ComponentsTotal: 6,
	}}}, d)
	assert.Equal(t, []string{statusPath}, f.paths())
}

func TestStatusModeIsTheDefault(t *testing.T) {
	f := statusFake(t, "status_operational.json")
	c := newCollector(t, instance("prod", f, ""))

	d := collect(t, c)

	assert.Equal(t, "operational", d.Instances[0].Status)
	assert.Equal(t, []string{statusPath}, f.paths())
}

func TestStatusModeWithIncident(t *testing.T) {
	c := newCollector(t, instance("prod", statusFake(t, "status_incident.json"), "status"))

	d := collect(t, c)

	assert.Equal(t, Instance{
		Name:            "prod",
		Status:          "partial_outage",
		Message:         "Some systems are experiencing issues",
		Incidents:       1,
		ComponentsDown:  2,
		ComponentsTotal: 6,
	}, d.Instances[0])
}

func TestStatusComponentsDown(t *testing.T) {
	comp := func(status string) string { return fmt.Sprintf(`{"name": "c", "status": %q}`, status) }
	tests := []struct {
		name       string
		components []string
		wantDown   int
	}{
		{"none", nil, 0},
		{"all operational", []string{comp("operational"), comp("operational")}, 0},
		{"degraded counts", []string{comp("operational"), comp("degraded")}, 1},
		{"every non operational status counts", []string{
			comp("degraded"), comp("partial_outage"), comp("major_outage"), comp("under_maintenance"), comp("operational"),
		}, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"global_status": "degraded", "components": [%s]}`, strings.Join(tt.components, ","))
			c := newCollector(t, instance("prod", newFake(t, map[string]string{statusPath: body}), "status"))

			in := collect(t, c).Instances[0]

			assert.Equal(t, tt.wantDown, in.ComponentsDown)
			assert.Equal(t, len(tt.components), in.ComponentsTotal)
		})
	}
}

func TestStatusStringIsPassedThrough(t *testing.T) {
	body := `{"global_status": "under_maintenance", "global_message": "Planned work", "components": [], "active_incidents": []}`
	c := newCollector(t, instance("prod", newFake(t, map[string]string{statusPath: body}), "status"))

	in := collect(t, c).Instances[0]

	assert.Equal(t, "under_maintenance", in.Status)
	assert.Equal(t, "Planned work", in.Message)
}

func TestAPIMode(t *testing.T) {
	tests := []struct {
		name       string
		alerts     string
		wantStatus string
		wantAlerts Alerts
	}{
		{"warnings degrade", "alerts_warning.json", "degraded", Alerts{Critical: 0, Warning: 2, Info: 1}},
		{"critical is a major outage", "alerts_critical.json", "major_outage", Alerts{Critical: 1, Warning: 1}},
		{"no alert is operational", "alerts_none.json", "operational", Alerts{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := apiFake(t, tt.alerts)
			c := newCollector(t, instance("prod", f, "api"))

			d := collect(t, c)

			assert.Equal(t, Data{Instances: []Instance{{
				Name:              "prod",
				Status:            tt.wantStatus,
				Alerts:            &tt.wantAlerts,
				Hosts:             ptr(4),
				ContainersRunning: ptr(21),
				ContainersTotal:   ptr(22),
			}}}, d)
			assert.Equal(t, []string{alertsPath, hostsPath, containersPath}, f.paths())
		})
	}
}

func TestAPIModeInfoAlertsDoNotDegrade(t *testing.T) {
	f := newFake(t, map[string]string{
		alertsPath:     `{"critical": [], "warning": [], "info": [{}, {}, {}]}`,
		hostsPath:      `{"hosts": []}`,
		containersPath: `{"groups": [], "total": 0}`,
	})
	c := newCollector(t, instance("prod", f, "api"))

	in := collect(t, c).Instances[0]

	assert.Equal(t, "operational", in.Status)
	assert.Equal(t, Alerts{Info: 3}, *in.Alerts)
	assert.Equal(t, 0, *in.Hosts)
	assert.Equal(t, 0, *in.ContainersTotal)
}

func TestAPIModeContainerTotalFallsBackToTheListedContainers(t *testing.T) {
	f := newFake(t, map[string]string{
		alertsPath: `{"critical": null, "warning": null, "info": null}`,
		hostsPath:  `{"hosts": [{"hostname": "a"}]}`,
		containersPath: `{"groups": [
			{"name": "a", "containers": [{"state": "running"}, {"state": "exited"}]},
			{"name": "b", "containers": [{"state": "running"}, {"state": "paused"}, {"state": "restarting"}]}
		]}`,
	})
	c := newCollector(t, instance("prod", f, "api"))

	in := collect(t, c).Instances[0]

	assert.Equal(t, 5, *in.ContainersTotal)
	assert.Equal(t, 2, *in.ContainersRunning)
	assert.Equal(t, "operational", in.Status)
}

func TestHeadersAreSentOnEveryRequest(t *testing.T) {
	f := apiFake(t, "alerts_none.json")
	in := instance("prod", f, "api")
	in.Headers = map[string]string{"CF-Access-Client-Id": "id-123", "CF-Access-Client-Secret": "s3cret", "Accept": "application/json; v=1"}
	c := newCollector(t, in)

	collect(t, c)

	reqs := f.requests()
	require.Len(t, reqs, 3)
	for _, r := range reqs {
		assert.Equal(t, "id-123", r.header.Get("CF-Access-Client-Id"), r.path)
		assert.Equal(t, "s3cret", r.header.Get("CF-Access-Client-Secret"), r.path)
		assert.Equal(t, "application/json; v=1", r.header.Get("Accept"), r.path)
		assert.Contains(t, r.header.Get("User-Agent"), "pi-dashboard", r.path)
	}
}

func TestTrailingSlashInURLIsIgnored(t *testing.T) {
	f := statusFake(t, "status_operational.json")
	in := instance("prod", f, "status")
	in.URL += "/"
	c := newCollector(t, in)

	collect(t, c)

	assert.Equal(t, []string{statusPath}, f.paths())
}

func TestFailedInstanceKeepsLastGoodValues(t *testing.T) {
	prod := statusFake(t, "status_operational.json")
	staging := statusFake(t, "status_incident.json")
	c := newCollector(t, instance("prod", prod, "status"), instance("staging", staging, "status"))
	first := collect(t, c)
	require.Empty(t, first.Instances[1].Error)

	staging.set(statusPath, http.StatusBadGateway, "<html>bad gateway</html>")
	second := collect(t, c)

	assert.Equal(t, first.Instances[0], second.Instances[0])
	got := second.Instances[1]
	assert.Contains(t, got.Error, "HTTP 502")
	want := first.Instances[1]
	want.Error = got.Error
	assert.Equal(t, want, got)

	staging.set(statusPath, http.StatusOK, fixture(t, "status_operational.json"))
	third := collect(t, c)

	assert.Empty(t, third.Instances[1].Error)
	assert.Equal(t, "operational", third.Instances[1].Status)
}

func TestInstanceFailingFromTheStartIsUnknown(t *testing.T) {
	prod := statusFake(t, "status_operational.json")
	broken := newFake(t, nil)
	c := newCollector(t, instance("prod", prod, "status"), instance("broken", broken, "status"))

	d := collect(t, c)

	assert.Equal(t, "operational", d.Instances[0].Status)
	got := d.Instances[1]
	assert.Equal(t, "broken", got.Name)
	assert.Equal(t, "unknown", got.Status)
	assert.Contains(t, got.Error, "HTTP 404")
	assert.Zero(t, got.ComponentsTotal)
}

func TestAPIModeAnyEndpointFailingFailsTheInstance(t *testing.T) {
	good := statusFake(t, "status_operational.json")
	api := apiFake(t, "alerts_none.json")
	api.set(hostsPath, http.StatusInternalServerError, "boom")
	c := newCollector(t, instance("prod", good, "status"), instance("edge", api, "api"))

	d := collect(t, c)

	edge := d.Instances[1]
	assert.Equal(t, "unknown", edge.Status)
	assert.Contains(t, edge.Error, "hosts")
	assert.Contains(t, edge.Error, "HTTP 500")
	assert.Nil(t, edge.Alerts)
	assert.Equal(t, []string{alertsPath, hostsPath}, api.paths())
}

func TestAllInstancesFailing(t *testing.T) {
	a, b := newFake(t, nil), newFake(t, nil)
	c := newCollector(t, instance("a", a, "status"), instance("b", b, "api"))

	got, err := c.Collect(context.Background())

	require.Error(t, err)
	assert.ErrorContains(t, err, "every instance failed")
	assert.ErrorContains(t, err, "a:")
	assert.Nil(t, got)
}

func TestForeignJSONIsRejected(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"unrelated json", `{"hello": "world"}`, "global_status"},
		{"empty object", `{}`, "global_status"},
		{"login page", `<html><title>Sign in</title></html>`, "decode"},
		{"empty body", ``, "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCollector(t, instance("prod", newFake(t, map[string]string{statusPath: tt.body}), "status"))

			_, err := c.Collect(context.Background())

			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestUnknownMode(t *testing.T) {
	c := newCollector(t, instance("prod", statusFake(t, "status_operational.json"), "graphql"))

	_, err := c.Collect(context.Background())

	require.ErrorContains(t, err, `unknown mode "graphql"`)
}

func TestNoInstances(t *testing.T) {
	c := newCollector(t)

	got, err := c.Collect(context.Background())

	require.ErrorContains(t, err, "no instances")
	assert.Nil(t, got)
}

func TestSlowInstanceDoesNotBlockTheOthers(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(hang.Close)
	prod := statusFake(t, "status_operational.json")
	c := newCollector(t,
		config.MaintenantInstance{Name: "slow", URL: hang.URL, Mode: "status"},
		instance("prod", prod, "status"),
	)
	c.timeout = 150 * time.Millisecond

	start := time.Now()
	d := collect(t, c)

	assert.Less(t, time.Since(start), 3*time.Second)
	assert.NotEmpty(t, d.Instances[0].Error)
	assert.Empty(t, d.Instances[1].Error)
	assert.Equal(t, "operational", d.Instances[1].Status)
}

func TestCanceledContext(t *testing.T) {
	c := newCollector(t, instance("prod", statusFake(t, "status_operational.json"), "status"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := c.Collect(ctx)

	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, got)
}

func TestTransportErrorDoesNotLeakTheURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c := newCollector(t, config.MaintenantInstance{Name: "prod", URL: strings.Replace(url, "http://", "http://bob:hunter2@", 1) + "/base", Mode: "status"})

	d, err := c.Collect(context.Background())

	require.Error(t, err)
	assert.Nil(t, d)
	assert.NotContains(t, err.Error(), "hunter2")
	assert.NotContains(t, err.Error(), "/base")
	assert.Contains(t, err.Error(), "refused")
}

func TestTimeoutScalesWithInstances(t *testing.T) {
	tests := []struct {
		instances int
		want      time.Duration
	}{
		{0, 15 * time.Second},
		{1, 15 * time.Second},
		{3, 35 * time.Second},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.instances), func(t *testing.T) {
			c := newCollector(t, make([]config.MaintenantInstance, tt.instances)...)

			assert.Equal(t, tt.want, c.Timeout())
		})
	}
}

func TestSummary(t *testing.T) {
	c := newCollector(t)
	tests := []struct {
		name string
		data any
		want string
	}{
		{
			name: "status instance with hosts",
			data: Data{Instances: []Instance{{Name: "prod", Status: "operational", Hosts: ptr(4)}}},
			want: "prod operational · 0 incident · 4 hosts",
		},
		{
			name: "plurals",
			data: Data{Instances: []Instance{{Name: "prod", Status: "partial_outage", Incidents: 2, Hosts: ptr(1)}}},
			want: "prod partial_outage · 2 incidents · 1 host",
		},
		{
			name: "api instance",
			data: Data{Instances: []Instance{{
				Name: "prod", Status: "degraded", Alerts: &Alerts{Critical: 0, Warning: 2, Info: 5},
				Hosts: ptr(4), ContainersRunning: ptr(21), ContainersTotal: ptr(22),
			}}},
			want: "prod degraded · 0 critical, 2 warning · 4 hosts · 21/22 containers",
		},
		{
			name: "failed instance among others",
			data: Data{Instances: []Instance{
				{Name: "prod", Status: "operational"},
				{Name: "edge", Status: "unknown", Error: "HTTP 502"},
			}},
			want: "prod operational · 0 incident; edge unknown · 0 incident · error: HTTP 502",
		},
		{name: "foreign data", data: nil, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, c.Summary(tt.data))
		})
	}
}

func TestIdentity(t *testing.T) {
	c := New(config.Maintenant{Interval: time.Minute}, collector.Deps{})

	assert.Equal(t, "maintenant", c.Name())
	assert.Equal(t, time.Minute, c.Interval())
}

func TestNilLoggerIsTolerated(t *testing.T) {
	c := New(config.Maintenant{Instances: []config.MaintenantInstance{
		instance("prod", statusFake(t, "status_operational.json"), "status"),
		instance("broken", newFake(t, nil), "status"),
	}}, collector.Deps{HTTP: http.DefaultClient})

	d := collect(t, c)

	assert.Equal(t, "unknown", d.Instances[1].Status)
}

func TestInvalidURLDoesNotLeakCredentials(t *testing.T) {
	c := newCollector(t,
		config.MaintenantInstance{Name: "prod", URL: "http://bob:hunter2@[::1", Mode: "status"},
		instance("ok", statusFake(t, "status_operational.json"), "status"),
	)

	d := collect(t, c)

	assert.Equal(t, "invalid url", d.Instances[0].Error)
	assert.Equal(t, "unknown", d.Instances[0].Status)
}

func TestAPIModeContainersFailureNamesTheEndpoint(t *testing.T) {
	good := statusFake(t, "status_operational.json")
	api := apiFake(t, "alerts_none.json")
	api.set(containersPath, http.StatusServiceUnavailable, "")
	c := newCollector(t, instance("prod", good, "status"), instance("edge", api, "api"))

	edge := collect(t, c).Instances[1]

	assert.Contains(t, edge.Error, "containers")
	assert.Contains(t, edge.Error, "HTTP 503")
}

func TestFailureIsLoggedOnceUntilRecovery(t *testing.T) {
	var logs strings.Builder
	f := statusFake(t, "status_operational.json")
	other := statusFake(t, "status_operational.json")
	c := newCollector(t, instance("prod", f, "status"), instance("other", other, "status"))
	c.deps.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	collect(t, c)
	assert.Empty(t, logs.String())

	f.set(statusPath, http.StatusBadGateway, "")
	collect(t, c)
	collect(t, c)
	collect(t, c)
	assert.Equal(t, 1, strings.Count(logs.String(), "maintenant instance failed"))
	assert.Contains(t, logs.String(), "level=WARN")
	assert.Contains(t, logs.String(), "instance=prod")
	assert.NotContains(t, logs.String(), "instance=other")

	f.set(statusPath, http.StatusOK, fixture(t, "status_operational.json"))
	collect(t, c)
	collect(t, c)
	assert.Equal(t, 1, strings.Count(logs.String(), "maintenant instance recovered"))

	f.set(statusPath, http.StatusBadGateway, "")
	collect(t, c)
	assert.Equal(t, 2, strings.Count(logs.String(), "maintenant instance failed"))
}
