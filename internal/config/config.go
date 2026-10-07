package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/kolapsis/pi-dashboard/internal/night"
)

type Config struct {
	Listen     string     `yaml:"listen"`
	DataDir    string     `yaml:"data_dir"`
	Timezone   string     `yaml:"timezone"`
	UI         UI         `yaml:"ui"`
	History    History    `yaml:"history"`
	Collectors Collectors `yaml:"collectors"`
}

type UI struct {
	Scale string `yaml:"scale"`
	Idle  Idle   `yaml:"idle"`
	Night Night  `yaml:"night"`
}

type Idle struct {
	Timeout time.Duration `yaml:"timeout"`
}

type Night struct {
	Enabled    *bool   `yaml:"enabled"`
	From       string  `yaml:"from"`
	To         string  `yaml:"to"`
	Brightness float64 `yaml:"brightness"`
	ScreenOff  *bool   `yaml:"screen_off"`
}

func (n Night) On() bool { return n.Enabled == nil || *n.Enabled }

func (n Night) Off() bool { return n.ScreenOff == nil || *n.ScreenOff }

type History struct {
	RetentionDays int `yaml:"retention_days"`
}

type Collectors struct {
	Mail       *Mail       `yaml:"mail"`
	GitHub     *GitHub     `yaml:"github"`
	Umami      *Umami      `yaml:"umami"`
	Stripe     *Stripe     `yaml:"stripe"`
	Qonto      *Qonto      `yaml:"qonto"`
	Weather    *Weather    `yaml:"weather"`
	Calendar   *Calendar   `yaml:"calendar"`
	Health     *Health     `yaml:"health"`
	Registry   *Registry   `yaml:"registry"`
	Maintenant *Maintenant `yaml:"maintenant"`
	JSONPoll   []JSONPoll  `yaml:"jsonpoll"`
}

type Mail struct {
	Interval    time.Duration `yaml:"interval"`
	Host        string        `yaml:"host"`
	User        string        `yaml:"user"`
	AppPassword string        `yaml:"app_password"`
	Mailbox     string        `yaml:"mailbox"`
	Latest      int           `yaml:"latest"`
}

type GitHub struct {
	Interval        time.Duration `yaml:"interval"`
	TrafficInterval time.Duration `yaml:"traffic_interval"`
	Org             string        `yaml:"org"`
	Token           string        `yaml:"token"`
	Repos           []string      `yaml:"repos"`
	TrafficRepos    []string      `yaml:"traffic_repos"`
	Exclude         []string      `yaml:"exclude"`
}

type Umami struct {
	Interval time.Duration `yaml:"interval"`
	BaseURL  string        `yaml:"base_url"`
	APIKey   string        `yaml:"api_key"`
	Username string        `yaml:"username"`
	Password string        `yaml:"password"`
	Sites    []UmamiSite   `yaml:"sites"`
}

type UmamiSite struct {
	ID    string `yaml:"id"`
	Label string `yaml:"label"`
}

type Stripe struct {
	Interval time.Duration   `yaml:"interval"`
	Accounts []StripeAccount `yaml:"accounts"`
}

type StripeAccount struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

type Qonto struct {
	Interval time.Duration `yaml:"interval"`
	Orgs     []QontoOrg    `yaml:"orgs"`
}

type QontoOrg struct {
	Name      string  `yaml:"name"`
	Slug      string  `yaml:"slug"`
	Secret    string  `yaml:"secret"`
	WarnBelow float64 `yaml:"warn_below"`
}

type Weather struct {
	Interval time.Duration `yaml:"interval"`
	Label    string        `yaml:"label"`
	Lat      float64       `yaml:"lat"`
	Lon      float64       `yaml:"lon"`
}

type Calendar struct {
	Interval      time.Duration  `yaml:"interval"`
	LookaheadDays int            `yaml:"lookahead_days"`
	Feeds         []CalendarFeed `yaml:"feeds"`
}

type CalendarFeed struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

type Health struct {
	Interval      time.Duration `yaml:"interval"`
	FailThreshold int           `yaml:"fail_threshold"`
	Sites         []HealthSite  `yaml:"sites"`
}

type HealthSite struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

type Registry struct {
	Interval time.Duration  `yaml:"interval"`
	Items    []RegistryItem `yaml:"items"`
}

type RegistryItem struct {
	Kind    string `yaml:"kind"`
	Name    string `yaml:"name"`
	Org     string `yaml:"org"`
	Package string `yaml:"package"`
	Repo    string `yaml:"repo"`
}

type Maintenant struct {
	Interval  time.Duration        `yaml:"interval"`
	Instances []MaintenantInstance `yaml:"instances"`
}

type MaintenantInstance struct {
	Name    string            `yaml:"name"`
	URL     string            `yaml:"url"`
	Mode    string            `yaml:"mode"`
	Headers map[string]string `yaml:"headers"`
}

type JSONPoll struct {
	Name      string            `yaml:"name"`
	Interval  time.Duration     `yaml:"interval"`
	URL       string            `yaml:"url"`
	Headers   map[string]string `yaml:"headers"`
	BasicAuth *BasicAuth        `yaml:"basic_auth"`
	Items     []JSONPollItem    `yaml:"items"`
}

type BasicAuth struct {
	User     string `yaml:"user"`
	Password string `yaml:"password"`
}

type JSONPollItem struct {
	Key     string `yaml:"key"`
	Label   string `yaml:"label"`
	Path    string `yaml:"path"`
	Format  string `yaml:"format"`
	History bool   `yaml:"history"`
}

const minInterval = 30 * time.Second

// Load reads and validates the YAML file. The returned warnings are non-fatal.
func Load(path string) (*Config, []string, error) {
	var warnings []string
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
		warnings = append(warnings, fmt.Sprintf("%s is readable by others (mode %04o), expected 0600", path, info.Mode().Perm()))
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := Parse(b)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, warnings, nil
}

func Parse(b []byte) (*Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func Default() *Config {
	return &Config{
		Listen:   "127.0.0.1:8080",
		DataDir:  "/var/lib/pi-dashboard",
		Timezone: "Europe/Paris",
		UI:       UI{Scale: "auto", Idle: Idle{Timeout: 10 * time.Minute}, Night: Night{From: "23:00", To: "07:00", Brightness: 0.35}},
		History:  History{RetentionDays: 400},
	}
}

func (c *Config) applyDefaults() {
	cs := &c.Collectors
	if m := cs.Mail; m != nil {
		m.Interval = clampInterval(m.Interval, time.Minute)
		m.Host = or(m.Host, "imap.gmail.com:993")
		m.Mailbox = or(m.Mailbox, "INBOX")
		if m.Latest <= 0 {
			m.Latest = 5
		}
	}
	if g := cs.GitHub; g != nil {
		g.Interval = clampInterval(g.Interval, 15*time.Minute)
		g.TrafficInterval = clampInterval(g.TrafficInterval, time.Hour)
	}
	if u := cs.Umami; u != nil {
		u.Interval = clampInterval(u.Interval, 2*time.Minute)
		u.BaseURL = strings.TrimRight(u.BaseURL, "/")
	}
	if s := cs.Stripe; s != nil {
		s.Interval = clampInterval(s.Interval, 5*time.Minute)
	}
	if q := cs.Qonto; q != nil {
		q.Interval = clampInterval(q.Interval, 5*time.Minute)
	}
	if w := cs.Weather; w != nil {
		w.Interval = clampInterval(w.Interval, 15*time.Minute)
		w.Label = or(w.Label, "Bordeaux")
		if w.Lat == 0 && w.Lon == 0 {
			w.Lat, w.Lon = 44.8378, -0.5792
		}
	}
	if cal := cs.Calendar; cal != nil {
		cal.Interval = clampInterval(cal.Interval, 5*time.Minute)
		if cal.LookaheadDays <= 0 {
			cal.LookaheadDays = 7
		}
	}
	if h := cs.Health; h != nil {
		h.Interval = clampInterval(h.Interval, time.Minute)
		if h.FailThreshold <= 0 {
			h.FailThreshold = 2
		}
	}
	if r := cs.Registry; r != nil {
		r.Interval = clampInterval(r.Interval, time.Hour)
	}
	if m := cs.Maintenant; m != nil {
		m.Interval = clampInterval(m.Interval, time.Minute)
		for i := range m.Instances {
			m.Instances[i].Mode = or(m.Instances[i].Mode, "status")
			m.Instances[i].URL = strings.TrimRight(m.Instances[i].URL, "/")
		}
	}
	for i := range cs.JSONPoll {
		cs.JSONPoll[i].Interval = clampInterval(cs.JSONPoll[i].Interval, 15*time.Minute)
	}
}

func clampInterval(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return max(d, minInterval)
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if c.Listen == "" {
		fail("listen is required")
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		fail("timezone: %v", err)
	}
	if t := c.UI.Idle.Timeout; t != 0 && t < time.Minute {
		fail("ui.idle.timeout must be 0 or at least 1m")
	}
	if c.UI.Night.On() {
		if err := (night.Window{From: c.UI.Night.From, To: c.UI.Night.To}).Validate(); err != nil {
			fail("ui.night: %v", err)
		}
		if c.UI.Night.Brightness <= 0 || c.UI.Night.Brightness > 1 {
			fail("ui.night.brightness must be in (0, 1]")
		}
	}
	if c.History.RetentionDays < 30 {
		fail("history.retention_days must be at least 30")
	}
	cs := c.Collectors
	if m := cs.Mail; m != nil && (m.User == "" || m.AppPassword == "") {
		fail("collectors.mail: user and app_password are required")
	}
	if g := cs.GitHub; g != nil && g.Org == "" && len(g.Repos) == 0 {
		fail("collectors.github: org or repos is required")
	}
	if u := cs.Umami; u != nil {
		if u.BaseURL == "" {
			fail("collectors.umami: base_url is required")
		}
		if u.APIKey == "" && (u.Username == "" || u.Password == "") {
			fail("collectors.umami: api_key or username/password is required")
		}
	}
	if s := cs.Stripe; s != nil {
		if len(s.Accounts) == 0 {
			fail("collectors.stripe: at least one account is required")
		}
		for i, a := range s.Accounts {
			if a.Name == "" || a.Key == "" {
				fail("collectors.stripe.accounts[%d]: name and key are required", i)
			}
		}
	}
	if q := cs.Qonto; q != nil {
		if len(q.Orgs) == 0 {
			fail("collectors.qonto: at least one org is required")
		}
		for i, o := range q.Orgs {
			if o.Name == "" || o.Slug == "" || o.Secret == "" {
				fail("collectors.qonto.orgs[%d]: name, slug and secret are required", i)
			}
		}
	}
	if cal := cs.Calendar; cal != nil {
		if len(cal.Feeds) == 0 {
			fail("collectors.calendar: at least one feed is required")
		}
		for i, f := range cal.Feeds {
			if f.URL == "" {
				fail("collectors.calendar.feeds[%d]: url is required", i)
			}
		}
	}
	if h := cs.Health; h != nil {
		for i, s := range h.Sites {
			if s.Name == "" || s.URL == "" {
				fail("collectors.health.sites[%d]: name and url are required", i)
			}
		}
	}
	if r := cs.Registry; r != nil {
		for i, it := range r.Items {
			switch it.Kind {
			case "ghcr":
				if it.Org == "" || it.Package == "" {
					fail("collectors.registry.items[%d]: ghcr needs org and package", i)
				}
			case "dockerhub":
				if !strings.Contains(it.Repo, "/") {
					fail("collectors.registry.items[%d]: dockerhub needs repo as namespace/name", i)
				}
			default:
				fail("collectors.registry.items[%d]: kind must be ghcr or dockerhub", i)
			}
			if it.Name == "" {
				fail("collectors.registry.items[%d]: name is required", i)
			}
		}
	}
	if m := cs.Maintenant; m != nil {
		for i, in := range m.Instances {
			if in.Name == "" || in.URL == "" {
				fail("collectors.maintenant.instances[%d]: name and url are required", i)
			}
			if in.Mode != "status" && in.Mode != "api" {
				fail("collectors.maintenant.instances[%d]: mode must be status or api", i)
			}
		}
	}
	seen := map[string]bool{}
	for i, j := range cs.JSONPoll {
		if j.Name == "" || j.URL == "" {
			fail("collectors.jsonpoll[%d]: name and url are required", i)
		}
		if seen[j.Name] {
			fail("collectors.jsonpoll[%d]: duplicate name %q", i, j.Name)
		}
		seen[j.Name] = true
		if len(j.Items) == 0 {
			fail("collectors.jsonpoll[%d]: at least one item is required", i)
		}
		for k, it := range j.Items {
			if it.Key == "" || it.Path == "" {
				fail("collectors.jsonpoll[%d].items[%d]: key and path are required", i, k)
			}
		}
	}
	return errors.Join(errs...)
}

// IsNotExist reports whether err means the config file is missing.
func IsNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
