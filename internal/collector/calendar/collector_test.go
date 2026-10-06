package calendar

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/httpx"
	"github.com/kolapsis/pi-dashboard/internal/store"
)

var (
	_ collector.Collector  = (*Collector)(nil)
	_ collector.Summarizer = (*Collector)(nil)

	paris = mustLocation("Europe/Paris")
)

const (
	secret    = "private-9f3a7c1e5b"
	feedPath  = "/calendar/ical/benjamin%40kolapsis.com/" + secret + "/basic.ics"
	layout    = "2006-01-02T15:04-07:00"
	feedName  = "kOlapsis"
	otherName = "Perso"
)

func mustLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func oct(day, hour, min int) time.Time {
	return time.Date(2026, time.October, day, hour, min, 0, 0, paris)
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return string(b)
}

func serveICS(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/calendar; charset=UTF-8")
		_, _ = io.WriteString(w, body)
	}
}

func serveStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "calendar not found", code)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type site struct {
	mux  *http.ServeMux
	srv  *httptest.Server
	logs *syncBuffer
}

func newSite(t *testing.T) *site {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &site{mux: mux, srv: srv, logs: &syncBuffer{}}
}

func (s *site) feed(name, path string, h http.HandlerFunc) config.CalendarFeed {
	s.mux.HandleFunc(path, h)
	return config.CalendarFeed{Name: name, URL: s.srv.URL + path}
}

func (s *site) collector(now time.Time, feeds ...config.CalendarFeed) *Collector {
	return New(config.Calendar{LookaheadDays: 7, Feeds: feeds}, collector.Deps{
		HTTP:  s.srv.Client(),
		Clock: clock.NewFake(now),
		Hist:  store.NewMem(),
		Log:   slog.New(slog.NewTextHandler(s.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Loc:   paris,
	})
}

func collectFixture(t *testing.T, name string, now time.Time) Data {
	t.Helper()
	s := newSite(t)
	c := s.collector(now, s.feed(feedName, feedPath, serveICS(fixture(t, name))))

	got, err := c.Collect(context.Background())

	require.NoError(t, err)
	return got.(Data)
}

func lines(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		line := fmt.Sprintf("%s %s → %s", e.Title, e.Start.Format(layout), e.End.Format(layout))
		if e.AllDay {
			line = "* " + line
		}
		if e.InProgress {
			line += " [en cours]"
		}
		out = append(out, line)
	}
	return out
}

func TestCollectRecurringSeries(t *testing.T) {
	got := collectFixture(t, "recurring.ics", oct(21, 10, 30))

	assert.Equal(t, []string{
		"Stand-up 2026-10-21T09:00+02:00 → 2026-10-21T09:15+02:00",
		"Point quotidien 2026-10-21T11:00+02:00 → 2026-10-21T11:30+02:00",
	}, lines(got.Today))
	assert.Equal(t, []string{
		"Stand-up 2026-10-22T09:00+02:00 → 2026-10-22T09:15+02:00",
		"Cours d'anglais 2026-10-22T18:00+02:00 → 2026-10-22T19:00+02:00",
		"Point quotidien 2026-10-24T11:00+02:00 → 2026-10-24T11:30+02:00",
		"Stand-up 2026-10-27T09:00+01:00 → 2026-10-27T09:15+01:00",
	}, lines(got.Upcoming))
}

func TestCollectRecurringKeepsWallClockAcrossDST(t *testing.T) {
	got := collectFixture(t, "dst.ics", oct(25, 12, 0))

	require.Len(t, got.Upcoming, 2)
	revue := got.Upcoming[1]
	assert.Equal(t, "Revue hebdo", revue.Title)
	assert.Equal(t, "2026-10-26T10:00+01:00", revue.Start.Format(layout))
	assert.Equal(t, "09:00", revue.Start.UTC().Format("15:04"))
}

func TestCollectDayBoundariesOnDSTDay(t *testing.T) {
	got := collectFixture(t, "dst.ics", oct(25, 12, 0))

	assert.Equal(t, []string{
		"Nuit du changement d'heure 2026-10-25T01:00+02:00 → 2026-10-25T04:00+01:00",
		"Messe de minuit 2026-10-25T23:30+01:00 → 2026-10-25T23:59+01:00",
	}, lines(got.Today))
	assert.Equal(t, []string{
		"Réveil tôt 2026-10-26T00:30+01:00 → 2026-10-26T01:00+01:00",
		"Revue hebdo 2026-10-26T10:00+01:00 → 2026-10-26T11:00+01:00",
	}, lines(got.Upcoming))
}

func TestCollectInProgressUsesElapsedTime(t *testing.T) {
	now := time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC)

	got := collectFixture(t, "dst.ics", now)

	require.NotEmpty(t, got.Today)
	nuit := got.Today[0]
	assert.Equal(t, "Nuit du changement d'heure", nuit.Title)
	assert.True(t, nuit.InProgress)
	assert.False(t, got.Today[1].InProgress)
}

func TestCollectOverrides(t *testing.T) {
	got := collectFixture(t, "overrides.ics", oct(21, 10, 30))

	assert.Empty(t, got.Today)
	assert.Equal(t, []string{
		"Visio Cap Digital (déplacée) 2026-10-23T11:00+02:00 → 2026-10-23T12:00+02:00",
		"Invitation ponctuelle 2026-10-24T10:00+02:00 → 2026-10-24T11:00+02:00",
		"Yoga 2026-10-27T18:00+01:00 → 2026-10-27T19:00+01:00",
	}, lines(got.Upcoming))
}

func TestCollectAllDay(t *testing.T) {
	tests := []struct {
		name         string
		now          time.Time
		wantToday    []string
		wantUpcoming []string
	}{
		{
			name: "before the clock change",
			now:  oct(21, 10, 30),
			wantToday: []string{
				"* Retraite 2026-10-19T00:00+02:00 → 2026-10-23T00:00+02:00",
				"* Vacances de Léa 2026-10-19T00:00+02:00 → 2026-10-23T00:00+02:00",
			},
			wantUpcoming: []string{
				"* Séminaire Biscuit 2026-10-23T00:00+02:00 → 2026-10-27T00:00+01:00",
				"* Week-end 2026-10-24T00:00+02:00 → 2026-10-26T00:00+01:00",
				"* Changement d'heure 2026-10-25T00:00+02:00 → 2026-10-26T00:00+01:00",
				"* Retraite 2026-10-26T00:00+01:00 → 2026-10-30T00:00+01:00",
				"* Ménage 2026-10-27T00:00+01:00 → 2026-10-28T00:00+01:00",
			},
		},
		{
			name: "on the clock change day",
			now:  oct(25, 12, 0),
			wantToday: []string{
				"* Séminaire Biscuit 2026-10-23T00:00+02:00 → 2026-10-27T00:00+01:00",
				"* Week-end 2026-10-24T00:00+02:00 → 2026-10-26T00:00+01:00",
				"* Changement d'heure 2026-10-25T00:00+02:00 → 2026-10-26T00:00+01:00",
			},
			wantUpcoming: []string{
				"* Retraite 2026-10-26T00:00+01:00 → 2026-10-30T00:00+01:00",
				"* Ménage 2026-10-27T00:00+01:00 → 2026-10-28T00:00+01:00",
				"* Week-end 2026-10-31T00:00+01:00 → 2026-11-02T00:00+01:00",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collectFixture(t, "allday.ics", tt.now)

			assert.Equal(t, tt.wantToday, lines(got.Today))
			assert.Equal(t, tt.wantUpcoming, lines(got.Upcoming))
			for _, e := range slices.Concat(got.Today, got.Upcoming) {
				assert.False(t, e.InProgress, e.Title)
			}
		})
	}
}

func TestCollectSingleEvents(t *testing.T) {
	got := collectFixture(t, "misc.ics", oct(21, 10, 30))

	assert.Equal(t, []string{
		"Minuit pile 2026-10-21T00:00+02:00 → 2026-10-21T00:00+02:00",
		"(sans titre) 2026-10-21T08:00+02:00 → 2026-10-21T08:30+02:00",
		"Revue de code 2026-10-21T10:00+02:00 → 2026-10-21T11:00+02:00 [en cours]",
		"Déjeuner, Bordeaux; salle A 2026-10-21T12:30+02:00 → 2026-10-21T13:30+02:00",
		"Déjeuner équipe 2026-10-21T14:30+02:00 → 2026-10-21T15:30+02:00",
		"Rappel facture 2026-10-21T16:00+02:00 → 2026-10-21T16:00+02:00",
		"Astreinte 2026-10-21T23:00+02:00 → 2026-10-22T02:00+02:00",
	}, lines(got.Today))
	assert.Equal(t, []string{
		"Café 2026-10-22T09:30+02:00 → 2026-10-22T10:15+02:00",
		"Webinaire 2026-10-22T11:00+02:00 → 2026-10-22T12:00+02:00",
		"Dernière minute 2026-10-27T23:30+01:00 → 2026-10-27T23:59+01:00",
	}, lines(got.Upcoming))
	assert.Equal(t, "12 rue Vernier, 75017 Paris, France", got.Today[3].Location)
	assert.Empty(t, got.Today[2].Location)
}

func TestCollectOrderingAndCap(t *testing.T) {
	got := collectFixture(t, "ordering.ics", oct(21, 10, 30))

	assert.Equal(t, []string{
		"* Tout-jour depuis hier 2026-10-20T00:00+02:00 → 2026-10-22T00:00+02:00",
		"* Tout-jour aujourd'hui 2026-10-21T00:00+02:00 → 2026-10-22T00:00+02:00",
		"Nuit prolongée 2026-10-20T23:00+02:00 → 2026-10-21T01:00+02:00",
		"Matin 2026-10-21T08:00+02:00 → 2026-10-21T09:00+02:00",
		"Soir 2026-10-21T19:00+02:00 → 2026-10-21T20:00+02:00",
	}, lines(got.Today))
	assert.Equal(t, []string{
		"* A all day 2026-10-22T00:00+02:00 → 2026-10-23T00:00+02:00",
		"B timed at midnight 2026-10-22T00:00+02:00 → 2026-10-22T00:30+02:00",
		"C 2026-10-22T08:00+02:00 → 2026-10-22T09:00+02:00",
		"D 2026-10-22T08:00+02:00 → 2026-10-22T09:00+02:00",
		"E 2026-10-22T14:00+02:00 → 2026-10-22T15:00+02:00",
		"F 2026-10-23T10:00+02:00 → 2026-10-23T11:00+02:00",
	}, lines(got.Upcoming))
}

func TestCollectGoogleExport(t *testing.T) {
	got := collectFixture(t, "google.ics", oct(21, 10, 30))

	assert.Equal(t, []string{
		"Point expert-comptable 2026-10-21T15:00+02:00 → 2026-10-21T15:45+02:00",
	}, lines(got.Today))
	assert.Equal(t, "Cabinet Delmas, 33000 Bordeaux, France", got.Today[0].Location)
	assert.Equal(t, []string{
		"Synchro bimensuelle 2026-10-22T09:30+02:00 → 2026-10-22T10:00+02:00",
		"* Anniversaire de Léa 2026-10-24T00:00+02:00 → 2026-10-25T00:00+02:00",
	}, lines(got.Upcoming))
}

func TestCollectEventsAreInTheConfiguredZone(t *testing.T) {
	got := collectFixture(t, "misc.ics", oct(21, 10, 30))

	for _, e := range slices.Concat(got.Today, got.Upcoming) {
		assert.Same(t, paris, e.Start.Location(), e.Title)
		assert.Same(t, paris, e.End.Location(), e.Title)
		assert.Equal(t, feedName, e.Calendar, e.Title)
	}
}

func TestCollectEmptyFeedIsNotNull(t *testing.T) {
	s := newSite(t)
	c := s.collector(oct(21, 10, 30), s.feed(feedName, feedPath, serveICS("BEGIN:VCALENDAR\nVERSION:2.0\nEND:VCALENDAR\n")))

	got, err := c.Collect(context.Background())

	require.NoError(t, err)
	b, err := json.Marshal(got)
	require.NoError(t, err)
	assert.JSONEq(t, `{"today": [], "upcoming": []}`, string(b))
}

func TestCollectMergesFeeds(t *testing.T) {
	s := newSite(t)
	c := s.collector(oct(21, 10, 30),
		s.feed(feedName, feedPath, serveICS(fixture(t, "recurring.ics"))),
		s.feed(otherName, "/perso/"+secret+"/basic.ics", serveICS(fixture(t, "dst.ics"))),
	)

	got, err := c.Collect(context.Background())

	require.NoError(t, err)
	d := got.(Data)
	require.Len(t, d.Today, 2)
	assert.Equal(t, feedName, d.Today[0].Calendar)
	require.Len(t, d.Upcoming, 6)
	byTitle := map[string]string{}
	for _, e := range d.Upcoming {
		byTitle[e.Title] = e.Calendar
	}
	assert.Equal(t, otherName, byTitle["Nuit du changement d'heure"])
	assert.Equal(t, feedName, byTitle["Cours d'anglais"])
}

func TestCollectSkipsFailingFeed(t *testing.T) {
	s := newSite(t)
	c := s.collector(oct(21, 10, 30),
		s.feed(feedName, feedPath, serveICS(fixture(t, "recurring.ics"))),
		s.feed(otherName, "/perso/"+secret+"/basic.ics", serveStatus(http.StatusNotFound)),
	)

	got, err := c.Collect(context.Background())

	require.NoError(t, err)
	assert.Len(t, got.(Data).Today, 2)
	logs := s.logs.String()
	assert.Contains(t, logs, "calendar feed failed")
	assert.Contains(t, logs, "feed="+otherName)
	assert.Contains(t, logs, "HTTP 404")
	assert.NotContains(t, logs, secret)
}

func TestCollectAllFeedsFail(t *testing.T) {
	t.Run("one feed", func(t *testing.T) {
		s := newSite(t)
		c := s.collector(oct(21, 10, 30), s.feed(feedName, feedPath, serveStatus(http.StatusNotFound)))

		got, err := c.Collect(context.Background())

		require.EqualError(t, err, feedName+": HTTP 404")
		assert.Nil(t, got)
	})
	t.Run("several feeds", func(t *testing.T) {
		s := newSite(t)
		c := s.collector(oct(21, 10, 30),
			s.feed(feedName, feedPath, serveStatus(http.StatusNotFound)),
			s.feed(otherName, "/perso/"+secret+"/basic.ics", serveStatus(http.StatusBadGateway)),
		)

		got, err := c.Collect(context.Background())

		require.EqualError(t, err, "all 2 calendar feeds failed: kOlapsis: HTTP 404; Perso: HTTP 502")
		assert.Nil(t, got)
		assert.NotContains(t, s.logs.String(), secret)
	})
	t.Run("no feed", func(t *testing.T) {
		s := newSite(t)

		_, err := s.collector(oct(21, 10, 30)).Collect(context.Background())

		require.ErrorContains(t, err, "no calendar feeds")
	})
}

func TestCollectErrorsDoNotLeakTheSecretURL(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		stop    bool
		want    string
	}{
		{name: "status", handler: serveStatus(http.StatusForbidden), want: "HTTP 403"},
		{name: "transport", handler: serveICS(""), stop: true, want: "connect"},
		{
			name: "truncated body",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "5000")
				_, _ = io.WriteString(w, "BEGIN:VCALENDAR\n")
				w.(http.Flusher).Flush()
				panic(http.ErrAbortHandler)
			},
			want: "read body",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSite(t)
			c := s.collector(oct(21, 10, 30), s.feed(feedName, feedPath, tt.handler))
			if tt.stop {
				s.srv.Close()
			}

			_, err := c.Collect(context.Background())

			require.ErrorContains(t, err, tt.want)
			assert.NotContains(t, err.Error(), secret)
			assert.NotContains(t, err.Error(), "benjamin")
			assert.NotContains(t, s.logs.String(), secret)
		})
	}
}

func TestCollectWebcalScheme(t *testing.T) {
	srv := httptest.NewTLSServer(serveICS(fixture(t, "recurring.ics")))
	t.Cleanup(srv.Close)
	c := New(config.Calendar{LookaheadDays: 7, Feeds: []config.CalendarFeed{
		{Name: feedName, URL: "webcal://" + strings.TrimPrefix(srv.URL, "https://") + feedPath},
	}}, collector.Deps{
		HTTP:  srv.Client(),
		Clock: clock.NewFake(oct(21, 10, 30)),
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Loc:   paris,
	})

	got, err := c.Collect(context.Background())

	require.NoError(t, err)
	assert.Len(t, got.(Data).Today, 2)
}

func TestCollectAcceptsBOMAndCRLF(t *testing.T) {
	body := "\xef\xbb\xbf" + strings.ReplaceAll(fixture(t, "recurring.ics"), "\n", "\r\n")
	s := newSite(t)
	c := s.collector(oct(21, 10, 30), s.feed(feedName, feedPath, serveICS(body)))

	got, err := c.Collect(context.Background())

	require.NoError(t, err)
	assert.Len(t, got.(Data).Today, 2)
}

func TestCollectMalformedFeeds(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"html page", "<!DOCTYPE html><html><body>Sign in</body></html>", "parse feed"},
		{"empty", "", "parse feed"},
		{"truncated", "BEGIN:VCALENDAR\nBEGIN:VEVENT\nUID:x\n", "parse feed"},
		{"parameter without value separator", "BEGIN:VCALENDAR\nBEGIN:VEVENT\nDTSTART;VALUE=DATE\nEND:VEVENT\nEND:VCALENDAR\n", "parse feed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSite(t)
			c := s.collector(oct(21, 10, 30), s.feed(feedName, feedPath, serveICS(tt.body)))

			got, err := c.Collect(context.Background())

			require.ErrorContains(t, err, tt.want)
			assert.Nil(t, got)
		})
	}
}

func TestCollectOneBadFeedDoesNotSpoilTheOthers(t *testing.T) {
	s := newSite(t)
	c := s.collector(oct(21, 10, 30),
		s.feed(otherName, "/perso/"+secret+"/basic.ics", serveICS("BEGIN:VCALENDAR\nBEGIN:VEVENT\nDTSTART;VALUE=DATE\nEND:VEVENT\nEND:VCALENDAR\n")),
		s.feed(feedName, feedPath, serveICS(fixture(t, "recurring.ics"))),
	)

	got, err := c.Collect(context.Background())

	require.NoError(t, err)
	assert.Len(t, got.(Data).Today, 2)
}

func TestCollectOversizedFeed(t *testing.T) {
	s := newSite(t)
	huge := "BEGIN:VCALENDAR\n" + strings.Repeat("X-FILLER:"+strings.Repeat("a", 1000)+"\n", httpx.MaxBody/1000+1) + "END:VCALENDAR\n"
	c := s.collector(oct(21, 10, 30), s.feed(feedName, feedPath, serveICS(huge)))

	_, err := c.Collect(context.Background())

	require.ErrorContains(t, err, "feed exceeds 8 MiB")
}

func TestCollectCanceledContext(t *testing.T) {
	s := newSite(t)
	c := s.collector(oct(21, 10, 30), s.feed(feedName, feedPath, serveICS(fixture(t, "recurring.ics"))))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Collect(ctx)

	require.ErrorIs(t, err, context.Canceled)
}

func TestCollectReportsSkippedEventsAtDebugLevel(t *testing.T) {
	s := newSite(t)
	c := s.collector(oct(21, 10, 30), s.feed(feedName, feedPath, serveICS(fixture(t, "misc.ics"))))

	_, err := c.Collect(context.Background())

	require.NoError(t, err)
	logs := s.logs.String()
	assert.Contains(t, logs, "calendar events skipped")
	assert.Contains(t, logs, "count=2")
}

func TestSummary(t *testing.T) {
	expertComptable := Event{Title: "Point expert-comptable", Start: oct(21, 15, 0), End: oct(21, 15, 45)}
	demo := Event{Title: "Démo Maintenant", Start: oct(21, 17, 30), End: oct(21, 18, 30)}
	visio := Event{Title: "Visio Cap Digital", Start: oct(22, 10, 0), End: oct(22, 11, 0)}
	banc := Event{Title: "Banc d'essai OpenSVC", Start: oct(23, 0, 0), End: oct(24, 0, 0), AllDay: true}
	holiday := Event{Title: "Vacances", Start: oct(19, 0, 0), End: oct(23, 0, 0), AllDay: true}
	tests := []struct {
		name string
		now  time.Time
		data any
		want string
	}{
		{"next event of the day", oct(21, 10, 30), Data{Today: []Event{expertComptable, demo}, Upcoming: []Event{visio}}, "2 aujourd'hui · prochain : Point expert-comptable 15:00"},
		{"first one is over", oct(21, 16, 0), Data{Today: []Event{expertComptable, demo}, Upcoming: []Event{visio}}, "2 aujourd'hui · prochain : Démo Maintenant 17:30"},
		{"event in progress is not next", oct(21, 15, 10), Data{Today: []Event{expertComptable, demo}}, "2 aujourd'hui · prochain : Démo Maintenant 17:30"},
		{"day is over", oct(21, 19, 0), Data{Today: []Event{expertComptable, demo}, Upcoming: []Event{visio}}, "2 aujourd'hui · prochain : Visio Cap Digital 22/10 10:00"},
		{"all-day event today is not next", oct(21, 10, 30), Data{Today: []Event{holiday}, Upcoming: []Event{banc}}, "1 aujourd'hui · prochain : Banc d'essai OpenSVC 23/10"},
		{"nothing coming", oct(21, 10, 30), Data{Today: []Event{holiday}}, "1 aujourd'hui"},
		{"empty", oct(21, 10, 30), Data{}, "0 aujourd'hui"},
		{"foreign data", oct(21, 10, 30), "not calendar data", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(config.Calendar{}, collector.Deps{Clock: clock.NewFake(tt.now), Loc: paris})

			assert.Equal(t, tt.want, c.Summary(tt.data))
		})
	}
}

func TestIdentity(t *testing.T) {
	c := New(config.Calendar{Interval: 5 * time.Minute}, collector.Deps{})

	assert.Equal(t, "calendar", c.Name())
	assert.Equal(t, 5*time.Minute, c.Interval())
}
