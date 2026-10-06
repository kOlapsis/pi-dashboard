package maintenant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/httpx"
)

const (
	instanceTimeout = 10 * time.Second

	statusOperational = "operational"
	statusDegraded    = "degraded"
	statusMajorOutage = "major_outage"
	statusUnknown     = "unknown"
)

type Collector struct {
	cfg     config.Maintenant
	deps    collector.Deps
	timeout time.Duration

	mu      sync.Mutex
	last    []*Instance
	failing []bool
}

func New(cfg config.Maintenant, deps collector.Deps) *Collector {
	if deps.Log == nil {
		deps.Log = slog.New(slog.DiscardHandler)
	}
	n := len(cfg.Instances)
	return &Collector{cfg: cfg, deps: deps, timeout: instanceTimeout, last: make([]*Instance, n), failing: make([]bool, n)}
}

func (c *Collector) Name() string { return "maintenant" }

func (c *Collector) Interval() time.Duration { return c.cfg.Interval }

func (c *Collector) Timeout() time.Duration {
	return time.Duration(max(len(c.cfg.Instances), 1))*c.timeout + 5*time.Second
}

func (c *Collector) Collect(ctx context.Context) (any, error) {
	instances := c.cfg.Instances
	if len(instances) == 0 {
		return nil, errors.New("no instances configured")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	d := Data{Instances: make([]Instance, len(instances))}
	failed := 0
	var first error
	for i, in := range instances {
		inst, err := c.poll(ctx, in)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			failed++
			c.logFailure(ctx, i, in.Name, err)
			if first == nil {
				first = fmt.Errorf("%s: %w", in.Name, err)
			}
			inst = c.lastGood(i, in.Name)
			inst.Error = err.Error()
		} else {
			if c.failing[i] {
				c.deps.Log.Info("maintenant instance recovered", "instance", in.Name)
			}
			c.failing[i] = false
			c.last[i] = &inst
		}
		d.Instances[i] = inst
	}
	if failed == len(instances) {
		return nil, fmt.Errorf("every instance failed, first: %w", first)
	}
	return d, nil
}

func (c *Collector) logFailure(ctx context.Context, i int, name string, err error) {
	level := slog.LevelDebug
	if !c.failing[i] {
		level = slog.LevelWarn
	}
	c.failing[i] = true
	c.deps.Log.Log(ctx, level, "maintenant instance failed", "instance", name, "err", err)
}

func (c *Collector) lastGood(i int, name string) Instance {
	if c.last[i] != nil {
		return *c.last[i]
	}
	return Instance{Name: name, Status: statusUnknown}
}

func (c *Collector) poll(ctx context.Context, in config.MaintenantInstance) (Instance, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	base := strings.TrimRight(in.URL, "/")
	switch in.Mode {
	case "status", "":
		return c.pollStatus(ctx, in, base)
	case "api":
		return c.pollAPI(ctx, in, base)
	}
	return Instance{}, fmt.Errorf("unknown mode %q", in.Mode)
}

func (c *Collector) pollStatus(ctx context.Context, in config.MaintenantInstance, base string) (Instance, error) {
	var page struct {
		GlobalStatus  string `json:"global_status"`
		GlobalMessage string `json:"global_message"`
		Components    []struct {
			Status string `json:"status"`
		} `json:"components"`
		ActiveIncidents []struct{} `json:"active_incidents"`
	}
	if err := c.get(ctx, in, base+"/status/api", &page); err != nil {
		return Instance{}, err
	}
	if page.GlobalStatus == "" {
		return Instance{}, errors.New("status response has no global_status")
	}
	inst := Instance{
		Name:            in.Name,
		Status:          page.GlobalStatus,
		Message:         page.GlobalMessage,
		Incidents:       len(page.ActiveIncidents),
		ComponentsTotal: len(page.Components),
	}
	for _, comp := range page.Components {
		if comp.Status != statusOperational {
			inst.ComponentsDown++
		}
	}
	return inst, nil
}

func (c *Collector) pollAPI(ctx context.Context, in config.MaintenantInstance, base string) (Instance, error) {
	var alerts struct {
		Critical []struct{} `json:"critical"`
		Warning  []struct{} `json:"warning"`
		Info     []struct{} `json:"info"`
	}
	if err := c.get(ctx, in, base+"/api/v1/alerts/active", &alerts); err != nil {
		return Instance{}, fmt.Errorf("alerts: %w", err)
	}
	var hosts struct {
		Hosts []struct{} `json:"hosts"`
	}
	if err := c.get(ctx, in, base+"/api/v1/resources/hosts", &hosts); err != nil {
		return Instance{}, fmt.Errorf("hosts: %w", err)
	}
	var containers struct {
		Groups []struct {
			Containers []struct {
				State string `json:"state"`
			} `json:"containers"`
		} `json:"groups"`
		Total *int `json:"total"`
	}
	if err := c.get(ctx, in, base+"/api/v1/containers", &containers); err != nil {
		return Instance{}, fmt.Errorf("containers: %w", err)
	}

	listed, running := 0, 0
	for _, g := range containers.Groups {
		listed += len(g.Containers)
		for _, ct := range g.Containers {
			if ct.State == "running" {
				running++
			}
		}
	}
	total := listed
	if containers.Total != nil {
		total = *containers.Total
	}
	nHosts := len(hosts.Hosts)

	a := &Alerts{Critical: len(alerts.Critical), Warning: len(alerts.Warning), Info: len(alerts.Info)}
	status := statusOperational
	switch {
	case a.Critical > 0:
		status = statusMajorOutage
	case a.Warning > 0:
		status = statusDegraded
	}
	return Instance{
		Name:              in.Name,
		Status:            status,
		Alerts:            a,
		Hosts:             &nHosts,
		ContainersRunning: &running,
		ContainersTotal:   &total,
	}, nil
}

func (c *Collector) get(ctx context.Context, in config.MaintenantInstance, rawURL string, out any) error {
	if _, err := url.Parse(rawURL); err != nil {
		return errors.New("invalid url")
	}
	err := httpx.GetJSON(ctx, c.deps.HTTP, rawURL, in.Headers, out)
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func (c *Collector) Summary(data any) string {
	d, ok := data.(Data)
	if !ok {
		return ""
	}
	lines := make([]string, len(d.Instances))
	for i, in := range d.Instances {
		parts := []string{in.Name + " " + in.Status}
		if in.Alerts == nil {
			parts = append(parts, count(in.Incidents, "incident"))
		} else {
			parts = append(parts, fmt.Sprintf("%d critical, %d warning", in.Alerts.Critical, in.Alerts.Warning))
		}
		if in.Hosts != nil {
			parts = append(parts, count(*in.Hosts, "host"))
		}
		if in.ContainersRunning != nil && in.ContainersTotal != nil {
			parts = append(parts, fmt.Sprintf("%d/%d containers", *in.ContainersRunning, *in.ContainersTotal))
		}
		if in.Error != "" {
			parts = append(parts, "error: "+in.Error)
		}
		lines[i] = strings.Join(parts, " · ")
	}
	return strings.Join(lines, "; ")
}

func count(n int, noun string) string {
	if n > 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s", n, noun)
}
