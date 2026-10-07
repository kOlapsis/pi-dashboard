package demo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/collector/calendar"
	"github.com/kolapsis/pi-dashboard/internal/collector/github"
	"github.com/kolapsis/pi-dashboard/internal/collector/health"
	"github.com/kolapsis/pi-dashboard/internal/collector/jsonpoll"
	"github.com/kolapsis/pi-dashboard/internal/collector/mail"
	"github.com/kolapsis/pi-dashboard/internal/collector/maintenant"
	"github.com/kolapsis/pi-dashboard/internal/collector/qonto"
	"github.com/kolapsis/pi-dashboard/internal/collector/registry"
	"github.com/kolapsis/pi-dashboard/internal/collector/stripe"
	"github.com/kolapsis/pi-dashboard/internal/collector/umami"
	"github.com/kolapsis/pi-dashboard/internal/collector/weather"
	"github.com/kolapsis/pi-dashboard/internal/store"
)

// Mode changes how a demo collector misbehaves: "" (healthy), "fail" (never succeeds) or "stale" (succeeds once, then fails).
type Mode string

type Collector struct {
	name     string
	interval time.Duration
	mode     Mode
	clock    clock.Clock
	gen      func(step int, now time.Time) any
	notify   func(prev, cur any) []string

	mu   sync.Mutex
	step int
}

func (c *Collector) Name() string { return c.name }

func (c *Collector) Interval() time.Duration { return c.interval }

func (c *Collector) Collect(_ context.Context) (any, error) {
	c.mu.Lock()
	step := c.step
	c.step++
	c.mu.Unlock()
	switch {
	case c.mode == "fail", c.mode == "stale" && step > 0:
		return nil, errors.New("demo: simulated failure")
	}
	return c.gen(step, c.clock.Now()), nil
}

func (c *Collector) Summary(any) string { return "demo data" }

func (c *Collector) Notify(prev, cur any) []string {
	if c.notify == nil {
		return nil
	}
	return c.notify(prev, cur)
}

// Collectors returns one fake collector per source, with intervals short enough to watch the UI move.
func Collectors(clk clock.Clock, loc *time.Location, seed uint64, modes map[string]Mode) []collector.Collector {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	g := &gen{rng: rng, loc: loc}
	mk := func(name string, interval time.Duration, f func(step int, now time.Time) any, notify func(prev, cur any) []string) collector.Collector {
		return &Collector{name: name, interval: interval, mode: modes[name], clock: clk, gen: f, notify: notify}
	}
	return []collector.Collector{
		mk("mail", 7*time.Second, g.mail, mail.Notify),
		mk("github", 11*time.Second, g.github, nil),
		mk("umami", 9*time.Second, g.umami, nil),
		mk("stripe", 13*time.Second, g.stripe, stripe.Notify),
		mk("qonto", 15*time.Second, g.qonto, qonto.Notify),
		mk("weather", 20*time.Second, g.weather, nil),
		mk("calendar", 17*time.Second, g.calendar, nil),
		mk("health", 8*time.Second, g.health, health.Notify),
		mk("registry", 30*time.Second, g.registry, nil),
		mk("maintenant", 10*time.Second, g.maintenant, nil),
		mk("shm", 30*time.Second, g.shm, nil),
	}
}

type gen struct {
	rng *rand.Rand
	loc *time.Location
}

func (g *gen) in(now time.Time) time.Time { return now.In(g.loc) }

func (g *gen) day(now time.Time) time.Time {
	n := g.in(now)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, g.loc)
}

func (g *gen) series(now time.Time, days int, end float64, perDay float64, noise float64) []store.Point {
	pts := make([]store.Point, 0, days+1)
	for i := days; i >= 0; i-- {
		v := end - perDay*float64(i) + noise*(g.rng.Float64()-0.5)
		pts = append(pts, store.Point{T: now.Add(-time.Duration(i) * 24 * time.Hour).Truncate(time.Hour), V: math.Round(v)})
	}
	return pts
}

func (g *gen) bars(now time.Time, days int, base float64, spread float64) []collector.DayPoint {
	out := make([]collector.DayPoint, 0, days)
	for i := days - 1; i >= 0; i-- {
		d := g.in(now).AddDate(0, 0, -i)
		wk := 1.0
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			wk = 0.55
		}
		out = append(out, collector.DayPoint{D: d.Format("2006-01-02"), V: math.Round(base*wk + spread*(g.rng.Float64()-0.5))})
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func (g *gen) mail(step int, now time.Time) any {
	senders := []struct{ from, subject string }{
		{"Julien Roux", "Re: Convention HOX – pièces complémentaires"},
		{"Stripe", "Paiement reçu : 29,00 € – Maintenant Pro"},
		{"Claire Morel", "Prévisionnel V3 : remarques avant dépôt"},
		{"GitHub", "[kOlapsis/maintenant] Issue #312 : failover sur Postgres"},
		{"Qonto", "Virement entrant de 447,00 €"},
		{"Cap Digital", "Comité de suivi : créneaux de novembre"},
		{"Umami", "Rapport hebdomadaire – maintenant.dev"},
		{"Initiative Gironde", "Votre dossier de prêt d'honneur"},
	}
	unseen := 4 + (step/4)%5
	day := g.day(now)
	items := make([]mail.Item, 0, 5)
	for i := 0; i < 5 && i < len(senders); i++ {
		k := (i + step/4) % len(senders)
		s := senders[k]
		items = append(items, mail.Item{From: s.from, Subject: s.subject, At: day.Add(8*time.Hour - time.Duration(k*37)*time.Minute)})
	}
	return mail.Data{Account: "benjamin@kolapsis.com", Unseen: unseen, Items: items}
}

func (g *gen) github(step int, now time.Time) any {
	stars := 528 + step/5
	repos := []github.Repo{
		{Name: "maintenant", Stars: stars, Delta7: ptr(12), Forks: 31, Issues: 9, PRs: 2, PushedAt: now.Add(-3 * time.Hour), Views14: ptr(2140), Clones14: ptr(338)},
		{Name: "ackify", Stars: 209, Delta7: ptr(1), Forks: 12, Issues: 3, PRs: 0, PushedAt: now.Add(-40 * 24 * time.Hour), Views14: ptr(260), Clones14: ptr(41)},
		{Name: "shm", Stars: 166, Delta7: ptr(3), Forks: 8, Issues: 2, PRs: 1, PushedAt: now.Add(-9 * 24 * time.Hour), Views14: ptr(410), Clones14: ptr(57)},
		{Name: "gofact", Stars: 5, Delta7: ptr(0), Forks: 0, Issues: 1, PRs: 0, PushedAt: now.Add(-2 * 24 * time.Hour)},
		{Name: "speckit-guard", Stars: 1, Delta7: ptr(0), Forks: 0, Issues: 0, PRs: 0, PushedAt: now.Add(-6 * 24 * time.Hour)},
	}
	total := 0
	for _, r := range repos {
		total += r.Stars
	}
	return github.Data{Stars: total, Delta7: ptr(16), Series30: g.series(now, 30, float64(total), 1.7, 2), Repos: repos, TokenExpiresAt: ptr(now.Add(300 * 24 * time.Hour))}
}

func (g *gen) umami(step int, now time.Time) any {
	pct := func(cur, prev int) *float64 {
		if prev == 0 {
			return nil
		}
		return ptr(math.Round(float64(cur-prev)/float64(prev)*1000) / 10)
	}
	sites := []umami.Site{
		{ID: "95bc46c9", Name: "maintenant.dev", Visits7: 812 + step%7, Prev7: 745, Pageviews7: 2210, Live: (step + 1) % 4, Bars14: g.bars(now, 14, 118, 30)},
		{ID: "a1", Name: "restoreproof.io", Visits7: 240, Prev7: 251, Pageviews7: 610, Live: step % 2, Bars14: g.bars(now, 14, 35, 12)},
		{ID: "b2", Name: "kolapsis.com", Visits7: 96, Prev7: 71, Pageviews7: 180, Live: 0, Bars14: g.bars(now, 14, 14, 8)},
	}
	d := umami.Data{Bars14: make([]collector.DayPoint, 14)}
	for _, s := range sites {
		s.DeltaPct = pct(s.Visits7, s.Prev7)
		d.Visits7 += s.Visits7
		d.Prev7 += s.Prev7
		d.Live += s.Live
		for i := range s.Bars14 {
			d.Bars14[i].D = s.Bars14[i].D
			d.Bars14[i].V += s.Bars14[i].V
		}
		d.Sites = append(d.Sites, s)
	}
	d.DeltaPct = pct(d.Visits7, d.Prev7)
	return d
}

func (g *gen) stripe(step int, now time.Time) any {
	mrr := int64(20300)
	accounts := []stripe.Account{
		{Name: "Maintenant", Livemode: true, MRRCents: mrr, Subs: 7, MonthNetCents: 44700 + int64(step/6)*2900, Payments: 3 + step/6, AvailableCents: 31240, PendingCents: 14900,
			Last: &stripe.Payment{AmountCents: 2900, Currency: "eur", Label: "Maintenant Pro", At: g.day(now).Add(9*time.Hour + time.Duration(step/8)*time.Minute)}},
		{Name: "RestoreProof", Livemode: true, Last: nil},
		{Name: "Ackify", Livemode: true, Last: nil},
	}
	var t stripe.Totals
	for _, a := range accounts {
		t.MRRCents += a.MRRCents
		t.MonthNetCents += a.MonthNetCents
		t.Payments += a.Payments
		t.AvailableCents += a.AvailableCents
		t.PendingCents += a.PendingCents
	}
	t.MRRDelta30Cents = ptr(int64(5800))
	return stripe.Data{Currency: "eur", Total: t, Series30: g.series(now, 30, float64(mrr), 190, 0), Accounts: accounts}
}

func (g *gen) qonto(step int, now time.Time) any {
	day := g.day(now)
	sas := qonto.Org{
		Name:       "kOlapsis SAS",
		TotalCents: 1248032 - int64(step)*1200,
		Accounts:   []qonto.Account{{Name: "Compte principal", BalanceCents: 1248032 - int64(step)*1200, AuthorizedCents: 1239032, Main: true}},
		Month:      qonto.Month{InCents: 44700, OutCents: 206498},
		Recent: []qonto.Transaction{
			{Label: "Stripe Payments UK Ltd", AmountCents: 44700, Side: "credit", Type: "income", At: day.Add(4*time.Hour + time.Duration(step/10)*time.Minute)},
			{Label: "OVH SAS", AmountCents: 2398, Side: "debit", Type: "direct_debit", At: day.Add(-2 * 24 * time.Hour)},
			{Label: "Cabinet Delmas", AmountCents: 42000, Side: "debit", Type: "transfer", At: day.Add(-3 * 24 * time.Hour)},
			{Label: "URSSAF", AmountCents: 61200, Side: "debit", Type: "direct_debit", At: day.Add(-5 * 24 * time.Hour)},
		},
	}
	ei := qonto.Org{
		Name:       "EI",
		TotalCents: 321010,
		Accounts:   []qonto.Account{{Name: "Compte courant", BalanceCents: 321010, AuthorizedCents: 321010, Main: true}},
		Month:      qonto.Month{InCents: 225000, OutCents: 61240},
		Recent: []qonto.Transaction{
			{Label: "Virement client", AmountCents: 225000, Side: "credit", Type: "income", At: day.Add(-4 * 24 * time.Hour)},
			{Label: "URSSAF", AmountCents: 56200, Side: "debit", Type: "direct_debit", At: day.Add(-6 * 24 * time.Hour)},
		},
	}
	total := sas.TotalCents + ei.TotalCents
	return qonto.Data{TotalCents: total, Series30: g.series(now, 30, float64(total), -3200, 20000), Orgs: []qonto.Org{sas, ei}}
}

func (g *gen) weather(_ int, now time.Time) any {
	n := g.in(now)
	h := n.Hour()
	code := 3
	switch {
	case h >= 7 && h < 11:
		code = 45
	case h >= 11 && h < 17:
		code = 2
	}
	temp := 10 + 6*math.Sin((float64(h)-6)/24*math.Pi)
	day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, g.loc)
	return weather.Data{
		Label: "Bordeaux", Temp: math.Round(temp*10) / 10, Feels: math.Round((temp-1.2)*10) / 10, Code: code, IsDay: h >= 8 && h < 19,
		TMin: 9, TMax: 17, RainPct: 20,
		Sunrise: day.Add(8*time.Hour + 5*time.Minute), Sunset: day.Add(19*time.Hour + 21*time.Minute),
		Tomorrow: &weather.Day{Code: 61, TMin: 11, TMax: 16, RainPct: 70},
	}
}

func (g *gen) calendar(_ int, now time.Time) any {
	n := g.in(now)
	day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, g.loc)
	ev := func(title string, start time.Time, d time.Duration, cal string) calendar.Event {
		e := calendar.Event{Title: title, Start: start, End: start.Add(d), Calendar: cal}
		e.InProgress = !now.Before(e.Start) && now.Before(e.End)
		return e
	}
	today := []calendar.Event{
		ev("Point expert-comptable", day.Add(15*time.Hour), 45*time.Minute, "kOlapsis"),
		ev("Démo Maintenant · pilote 2", day.Add(17*time.Hour+30*time.Minute), time.Hour, "kOlapsis"),
	}
	upcoming := []calendar.Event{
		ev("Visio Cap Digital · suivi", day.Add(24*time.Hour+10*time.Hour), time.Hour, "kOlapsis"),
		{Title: "Banc d'essai OpenSVC", Start: day.Add(48 * time.Hour), End: day.Add(72 * time.Hour), AllDay: true, Calendar: "kOlapsis"},
		ev("Dentiste", day.Add(72*time.Hour+9*time.Hour), 30*time.Minute, "Perso"),
	}
	return calendar.Data{Today: today, Upcoming: upcoming}
}

func (g *gen) health(step int, _ time.Time) any {
	sites := []health.Site{
		{Name: "maintenant.dev", URL: "https://maintenant.dev", Up: true, Status: 200, LatencyMs: 180 + g.rng.IntN(60), CertDays: ptr(61)},
		{Name: "demo", URL: "https://demo.maintenant.dev", Up: true, Status: 200, LatencyMs: 210 + g.rng.IntN(80), CertDays: ptr(61)},
		{Name: "restoreproof.io", URL: "https://restoreproof.io", Up: step%9 != 4, Status: 200, LatencyMs: 95 + g.rng.IntN(40), CertDays: ptr(34)},
		{Name: "kolapsis.com", URL: "https://kolapsis.com", Up: true, Status: 200, LatencyMs: 70 + g.rng.IntN(30), CertDays: ptr(80)},
		{Name: "umami", URL: "https://umami.kolapsis.com", Up: true, Status: 200, LatencyMs: 120 + g.rng.IntN(50), CertDays: ptr(12)},
	}
	d := health.Data{Total: len(sites), Sites: sites}
	for i := range sites {
		if !sites[i].Up {
			sites[i].Status = 0
			sites[i].Error = "connection timed out"
			continue
		}
		d.Up++
	}
	return d
}

func (g *gen) registry(step int, now time.Time) any {
	return registry.Data{Items: []registry.Item{
		{Name: "maintenant", Kind: "ghcr", Total: 207451 + int64(step)*40, Delta7: ptr(int64(14805)), PerDay: g.bars(now, 30, 2100, 500)},
		{Name: "ackify", Kind: "dockerhub", Total: 7096, Delta7: ptr(int64(23)), Stars: 4},
	}}
}

func (g *gen) maintenant(step int, _ time.Time) any {
	return maintenant.Data{Instances: []maintenant.Instance{{
		Name: "prod", Status: "operational", Incidents: 0, ComponentsDown: 0, ComponentsTotal: 6,
		Alerts: &maintenant.Alerts{Critical: 0, Warning: step % 3, Info: 2},
		Hosts:  ptr(4), ContainersRunning: ptr(23), ContainersTotal: ptr(24),
	}}}
}

func (g *gen) shm(step int, _ time.Time) any {
	return jsonpoll.Data{Name: "shm", Items: []jsonpoll.Item{
		{Key: "instances", Label: "Instances", Value: float64(214 + step/10), Text: fmt.Sprint(214 + step/10), Delta7: ptr(6.0)},
		{Key: "active", Label: "Actives", Value: 131, Text: "131"},
	}}
}
