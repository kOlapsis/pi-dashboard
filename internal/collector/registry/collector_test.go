package registry

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

	t0 = time.Date(2026, 10, 7, 12, 30, 0, 0, time.UTC)

	ghcrItem = config.RegistryItem{Kind: "ghcr", Name: "maintenant", Org: "kolapsis", Package: "maintenant"}
	hubItem  = config.RegistryItem{Kind: "dockerhub", Name: "ackify", Repo: "kolapsis/ackify"}
)

const (
	ghcrPath = "/kolapsis/maintenant/pkgs/container/maintenant"
	hubPath  = "/v2/repositories/kolapsis/ackify/"
)

type page struct {
	status int
	body   string
}

type upstream struct {
	mu    sync.Mutex
	pages map[string]page
	paths []string
}

func (u *upstream) set(path string, status int, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.pages[path] = page{status, body}
}

func (u *upstream) requested() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...)
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.paths = append(u.paths, r.URL.Path)
	p, ok := u.pages[r.URL.Path]
	u.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(p.status)
	_, _ = io.WriteString(w, p.body)
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

type env struct {
	c    *Collector
	up   *upstream
	clk  *clock.Fake
	logs *syncBuffer
}

func newEnv(t *testing.T, items ...config.RegistryItem) *env {
	t.Helper()
	up := &upstream{pages: map[string]page{
		ghcrPath: {http.StatusOK, fixture(t, "ghcr_maintenant.html")},
		hubPath:  {http.StatusOK, fixture(t, "dockerhub_ackify.json")},
	}}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	e := &env{up: up, clk: clock.NewFake(t0), logs: &syncBuffer{}}
	e.c = New(config.Registry{Items: items}, collector.Deps{
		HTTP:  srv.Client(),
		Clock: e.clk,
		Hist:  store.NewMem(),
		Log:   slog.New(slog.NewTextHandler(e.logs, nil)),
		Loc:   time.UTC,
	})
	e.c.ghcrBase, e.c.hubBase = srv.URL, srv.URL
	return e
}

func (e *env) collect(t *testing.T) Data {
	t.Helper()
	got, err := e.c.Collect(context.Background())
	require.NoError(t, err)
	return got.(Data)
}

func withTotal(html string, total int) string {
	return strings.Replace(html, `title="207590"`, fmt.Sprintf(`title="%d"`, total), 1)
}

func TestParseGHCRFixture(t *testing.T) {
	total, perDay, err := parseGHCR([]byte(fixture(t, "ghcr_maintenant.html")))

	require.NoError(t, err)
	assert.EqualValues(t, 207590, total)
	require.Len(t, perDay, 30)
	assert.Equal(t, collector.DayPoint{D: "2026-09-07", V: 1241}, perDay[0])
	assert.Equal(t, collector.DayPoint{D: "2026-10-06", V: 2254}, perDay[29])
	for i := 1; i < len(perDay); i++ {
		assert.Less(t, perDay[i-1].D, perDay[i].D)
	}
}

func TestParseGHCR(t *testing.T) {
	const total = `<span class="x">Total downloads</span><h3 title="1,234,567">1.2M</h3>`
	tests := []struct {
		name      string
		page      string
		wantTotal int64
		wantDays  []collector.DayPoint
	}{
		{
			name:      "thousands separators are stripped",
			page:      total,
			wantTotal: 1234567,
		},
		{
			name:      "whitespace between label and value",
			page:      "<span>Total downloads</span>\n\n   <h3\n   title=\"42\">42</h3>",
			wantTotal: 42,
		},
		{
			name:      "extra attributes on the heading",
			page:      `<span>Total downloads</span> <h3 class="big" data-title="9" title="77" id="t">77</h3>`,
			wantTotal: 77,
		},
		{
			name:      "heading titles before the label are ignored",
			page:      `<h3 title="7">7</h3><span>Total downloads</span><h3 title="99">99</h3>`,
			wantTotal: 99,
		},
		{
			name:      "merge count before date",
			page:      total + `<rect x="1" data-merge-count="5" data-date="2026-01-02" width="2"></rect>`,
			wantTotal: 1234567,
			wantDays:  []collector.DayPoint{{D: "2026-01-02", V: 5}},
		},
		{
			name:      "date before merge count",
			page:      total + `<rect data-date="2026-01-02" x="1" data-merge-count="5"></rect>`,
			wantTotal: 1234567,
			wantDays:  []collector.DayPoint{{D: "2026-01-02", V: 5}},
		},
		{
			name:      "attributes spread over lines",
			page:      total + "<rect\n  data-date=\"2026-01-02\"\n  y=\"3\"\n  data-merge-count=\"5\"\n  fill=\"#fff\"></rect>",
			wantTotal: 1234567,
			wantDays:  []collector.DayPoint{{D: "2026-01-02", V: 5}},
		},
		{
			name: "descending input is sorted ascending",
			page: total +
				`<rect data-merge-count="3" data-date="2026-01-03"></rect>` +
				`<rect data-merge-count="2" data-date="2026-01-02"></rect>` +
				`<rect data-merge-count="1" data-date="2026-01-01"></rect>`,
			wantTotal: 1234567,
			wantDays:  []collector.DayPoint{{D: "2026-01-01", V: 1}, {D: "2026-01-02", V: 2}, {D: "2026-01-03", V: 3}},
		},
		{
			name: "repeated date keeps the last value",
			page: total +
				`<rect data-merge-count="3" data-date="2026-01-03"></rect>` +
				`<rect data-merge-count="4" data-date="2026-01-03"></rect>`,
			wantTotal: 1234567,
			wantDays:  []collector.DayPoint{{D: "2026-01-03", V: 4}},
		},
		{
			name: "elements missing one attribute are skipped",
			page: total +
				`<rect data-merge-count="3"></rect>` +
				`<rect data-date="2026-01-04"></rect>` +
				`<rect data-merge-count="" data-date="2026-01-05"></rect>` +
				`<rect data-merge-count="8" data-date="2026-01-06"></rect>`,
			wantTotal: 1234567,
			wantDays:  []collector.DayPoint{{D: "2026-01-06", V: 8}},
		},
		{
			name: "counts too large for a float are skipped",
			page: total +
				`<rect data-merge-count="` + strings.Repeat("9", 400) + `" data-date="2026-01-03"></rect>` +
				`<rect data-merge-count="8" data-date="2026-01-06"></rect>`,
			wantTotal: 1234567,
			wantDays:  []collector.DayPoint{{D: "2026-01-06", V: 8}},
		},
		{
			name:      "no series is not an error",
			page:      total,
			wantTotal: 1234567,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTotal, gotDays, err := parseGHCR([]byte(tt.page))

			require.NoError(t, err)
			assert.Equal(t, tt.wantTotal, gotTotal)
			assert.Equal(t, tt.wantDays, gotDays)
		})
	}
}

func TestParseGHCRKeepsLatestThirtyDays(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<span>Total downloads</span><h3 title="1">1</h3>`)
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for i := 44; i >= 0; i-- {
		fmt.Fprintf(&b, `<rect data-merge-count="%d" data-date="%s"></rect>`, i, start.AddDate(0, 0, i).Format("2006-01-02"))
	}

	_, perDay, err := parseGHCR([]byte(b.String()))

	require.NoError(t, err)
	require.Len(t, perDay, 30)
	assert.Equal(t, "2026-08-16", perDay[0].D)
	assert.Equal(t, "2026-09-14", perDay[29].D)
	assert.EqualValues(t, 44, perDay[29].V)
}

func TestParseGHCRLayoutChanged(t *testing.T) {
	tests := []struct {
		name string
		page string
	}{
		{"empty page", ""},
		{"fixture without the fragments", fixture(t, "ghcr_layout_changed.html")},
		{"label without a heading", `<span>Total downloads</span><div>207590</div>`},
		{"heading without a title", `<span>Total downloads</span><h3>208K</h3>`},
		{"abbreviated title", `<span>Total downloads</span><h3 title="208K">208K</h3>`},
		{"series without total", `<rect data-merge-count="5" data-date="2026-01-02"></rect>`},
		{"total too large", `<span>Total downloads</span><h3 title="99999999999999999999999">x</h3>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			total, perDay, err := parseGHCR([]byte(tt.page))

			require.ErrorIs(t, err, errLayoutChanged)
			require.ErrorContains(t, err, "layout changed")
			assert.Zero(t, total)
			assert.Nil(t, perDay)
		})
	}
}

func TestCollect(t *testing.T) {
	e := newEnv(t, ghcrItem, hubItem)

	d := e.collect(t)

	require.Len(t, d.Items, 2)
	g := d.Items[0]
	assert.Equal(t, "maintenant", g.Name)
	assert.Equal(t, "ghcr", g.Kind)
	assert.EqualValues(t, 207590, g.Total)
	assert.Nil(t, g.Delta7)
	assert.Len(t, g.PerDay, 30)
	assert.Zero(t, g.Stars)
	assert.Equal(t, Item{Name: "ackify", Kind: "dockerhub", Total: 7096, Stars: 4}, d.Items[1])
	assert.ElementsMatch(t, []string{ghcrPath, hubPath}, e.up.requested())
}

func TestCollectGHCRRepoOverride(t *testing.T) {
	for _, repo := range []string{"maintenant-ui", "kolapsis/maintenant-ui"} {
		t.Run(repo, func(t *testing.T) {
			item := ghcrItem
			item.Repo = repo
			e := newEnv(t, item)
			e.up.set("/kolapsis/maintenant-ui/pkgs/container/maintenant", http.StatusOK, fixture(t, "ghcr_maintenant.html"))

			d := e.collect(t)

			assert.EqualValues(t, 207590, d.Items[0].Total)
			assert.Equal(t, []string{"/kolapsis/maintenant-ui/pkgs/container/maintenant"}, e.up.requested())
		})
	}
}

func TestCollectRecordsHistory(t *testing.T) {
	e := newEnv(t, ghcrItem, hubItem)

	e.collect(t)

	v, ok, err := e.c.deps.Hist.At(context.Background(), "registry", "pulls.maintenant", t0)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.EqualValues(t, 207590, v)
	v, ok, err = e.c.deps.Hist.At(context.Background(), "registry", "pulls.ackify", t0)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.EqualValues(t, 7096, v)
}

func TestDelta7(t *testing.T) {
	e := newEnv(t, ghcrItem, hubItem)
	e.up.set(ghcrPath, http.StatusOK, withTotal(fixture(t, "ghcr_maintenant.html"), 192646))
	e.up.set(hubPath, http.StatusOK, `{"pull_count": 7073, "star_count": 4}`)
	first := e.collect(t)
	assert.Nil(t, first.Items[0].Delta7)
	assert.Nil(t, first.Items[1].Delta7)

	e.clk.Advance(24 * time.Hour)
	e.up.set(ghcrPath, http.StatusOK, withTotal(fixture(t, "ghcr_maintenant.html"), 194000))
	assert.Nil(t, e.collect(t).Items[0].Delta7, "one day of history is not a week")

	e.clk.Advance(6 * 24 * time.Hour)
	e.up.set(ghcrPath, http.StatusOK, fixture(t, "ghcr_maintenant.html"))
	e.up.set(hubPath, http.StatusOK, fixture(t, "dockerhub_ackify.json"))
	second := e.collect(t)

	require.NotNil(t, second.Items[0].Delta7)
	assert.EqualValues(t, 207590-192646, *second.Items[0].Delta7)
	require.NotNil(t, second.Items[1].Delta7)
	assert.EqualValues(t, 23, *second.Items[1].Delta7)
}

func TestFailedItemKeepsLastGoodValue(t *testing.T) {
	e := newEnv(t, ghcrItem, hubItem)
	e.collect(t)
	e.clk.Advance(time.Hour)
	e.up.set(ghcrPath, http.StatusInternalServerError, "boom")
	e.up.set(hubPath, http.StatusOK, `{"pull_count": 7100, "star_count": 5}`)

	d := e.collect(t)

	require.Len(t, d.Items, 2)
	assert.EqualValues(t, 207590, d.Items[0].Total)
	assert.Len(t, d.Items[0].PerDay, 30)
	assert.EqualValues(t, 7100, d.Items[1].Total)
	assert.Equal(t, 5, d.Items[1].Stars)
	assert.Contains(t, e.logs.String(), "registry item failed")
	assert.Contains(t, e.logs.String(), "item=maintenant")
	assert.Contains(t, e.logs.String(), "HTTP 500")
}

func TestLayoutChangeNeverReportsZero(t *testing.T) {
	e := newEnv(t, ghcrItem, hubItem)
	e.collect(t)
	e.up.set(ghcrPath, http.StatusOK, fixture(t, "ghcr_layout_changed.html"))

	d := e.collect(t)

	assert.EqualValues(t, 207590, d.Items[0].Total)
	assert.Contains(t, e.logs.String(), "layout changed")
}

func TestItemFailingFromTheStartIsOmitted(t *testing.T) {
	e := newEnv(t, ghcrItem, hubItem)
	e.up.set(ghcrPath, http.StatusNotFound, "")

	d := e.collect(t)

	require.Len(t, d.Items, 1)
	assert.Equal(t, "ackify", d.Items[0].Name)
}

func TestAllItemsFailing(t *testing.T) {
	e := newEnv(t, ghcrItem, hubItem)
	e.up.set(ghcrPath, http.StatusOK, fixture(t, "ghcr_layout_changed.html"))
	e.up.set(hubPath, http.StatusBadGateway, "")

	got, err := e.c.Collect(context.Background())

	require.Error(t, err)
	require.ErrorIs(t, err, errLayoutChanged)
	assert.ErrorContains(t, err, "every item failed")
	assert.ErrorContains(t, err, "maintenant")
	assert.Nil(t, got)
}

func TestAllItemsFailingKeepsNothingFromBefore(t *testing.T) {
	e := newEnv(t, ghcrItem)
	e.collect(t)
	e.up.set(ghcrPath, http.StatusInternalServerError, "")

	got, err := e.c.Collect(context.Background())

	require.Error(t, err)
	assert.Nil(t, got)
}

func TestDockerHubErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"missing pull count", 200, `{"star_count": 3}`, "pull_count missing"},
		{"null pull count", 200, `{"pull_count": null}`, "pull_count missing"},
		{"not json", 200, "<html>", "decode"},
		{"not found", 404, `{"message": "httperror 404: object not found"}`, "HTTP 404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, hubItem)
			e.up.set(hubPath, tt.status, tt.body)

			_, err := e.c.Collect(context.Background())

			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestDockerHubZeroPullsIsValid(t *testing.T) {
	e := newEnv(t, hubItem)
	e.up.set(hubPath, http.StatusOK, `{"pull_count": 0, "star_count": 0}`)

	d := e.collect(t)

	assert.Zero(t, d.Items[0].Total)
}

func TestUnknownKind(t *testing.T) {
	e := newEnv(t, config.RegistryItem{Kind: "quay", Name: "x"})

	_, err := e.c.Collect(context.Background())

	require.ErrorContains(t, err, `unknown kind "quay"`)
}

func TestNoItems(t *testing.T) {
	e := newEnv(t)

	got, err := e.c.Collect(context.Background())

	require.ErrorContains(t, err, "no items")
	assert.Nil(t, got)
}

func TestCanceledContext(t *testing.T) {
	e := newEnv(t, ghcrItem, hubItem)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := e.c.Collect(ctx)

	require.ErrorIs(t, err, context.Canceled)
}

func TestSummary(t *testing.T) {
	c := New(config.Registry{}, collector.Deps{})
	delta := func(v int64) *int64 { return &v }
	tests := []struct {
		name string
		data any
		want string
	}{
		{
			name: "with delta",
			data: Data{Items: []Item{{Name: "maintenant", Total: 207451, Delta7: delta(14805)}}},
			want: "maintenant 207 451 pulls (+14 805 / 7 j)",
		},
		{
			name: "without history",
			data: Data{Items: []Item{{Name: "maintenant", Total: 207451}}},
			want: "maintenant 207 451 pulls",
		},
		{
			name: "several items",
			data: Data{Items: []Item{
				{Name: "maintenant", Total: 207451, Delta7: delta(14805)},
				{Name: "ackify", Total: 7096, Delta7: delta(0)},
			}},
			want: "maintenant 207 451 pulls (+14 805 / 7 j) · ackify 7 096 pulls (+0 / 7 j)",
		},
		{
			name: "negative delta",
			data: Data{Items: []Item{{Name: "x", Total: 10, Delta7: delta(-1200)}}},
			want: "x 10 pulls (-1 200 / 7 j)",
		},
		{name: "empty", data: Data{}, want: ""},
		{name: "foreign data", data: 42, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, c.Summary(tt.data))
		})
	}
}

func TestGrouped(t *testing.T) {
	tests := []struct {
		n      int64
		signed bool
		want   string
	}{
		{0, false, "0"},
		{0, true, "+0"},
		{7, false, "7"},
		{999, false, "999"},
		{1000, false, "1 000"},
		{14805, true, "+14 805"},
		{207451, false, "207 451"},
		{1234567, false, "1 234 567"},
		{-120, true, "-120"},
		{-1200, false, "-1 200"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, grouped(tt.n, tt.signed))
		})
	}
}

func TestIdentity(t *testing.T) {
	c := New(config.Registry{Interval: time.Hour}, collector.Deps{})

	assert.Equal(t, "registry", c.Name())
	assert.Equal(t, time.Hour, c.Interval())
}

func TestNilLoggerIsTolerated(t *testing.T) {
	e := newEnv(t, ghcrItem, hubItem)
	e.up.set(ghcrPath, http.StatusInternalServerError, "")
	c := New(e.c.cfg, collector.Deps{HTTP: e.c.deps.HTTP, Clock: e.clk})
	c.ghcrBase, c.hubBase = e.c.ghcrBase, e.c.hubBase

	got, err := c.Collect(context.Background())

	require.NoError(t, err)
	assert.Len(t, got.(Data).Items, 1)
}

func TestInvalidBaseURLs(t *testing.T) {
	tests := []struct {
		name    string
		item    config.RegistryItem
		wantErr string
	}{
		{"ghcr", ghcrItem, "invalid ghcr url"},
		{"dockerhub", hubItem, "invalid dockerhub url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.item)
			e.c.ghcrBase, e.c.hubBase = "http://[::1", "http://[::1"

			_, err := e.c.Collect(context.Background())

			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestTimeoutScalesWithItems(t *testing.T) {
	tests := []struct {
		items int
		want  time.Duration
	}{
		{0, 20 * time.Second},
		{1, 20 * time.Second},
		{4, 20 * time.Second},
		{5, 35 * time.Second},
		{9, 50 * time.Second},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.items), func(t *testing.T) {
			c := New(config.Registry{Items: make([]config.RegistryItem, tt.items)}, collector.Deps{})

			assert.Equal(t, tt.want, c.Timeout())
		})
	}
}
