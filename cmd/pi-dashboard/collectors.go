package main

import (
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
	"github.com/kolapsis/pi-dashboard/internal/config"
)

func buildCollectors(cfg *config.Config, deps collector.Deps) []collector.Collector {
	var out []collector.Collector
	cs := cfg.Collectors
	if cs.Mail != nil {
		out = append(out, mail.New(*cs.Mail, deps))
	}
	if cs.GitHub != nil {
		out = append(out, github.New(*cs.GitHub, deps))
	}
	if cs.Umami != nil {
		out = append(out, umami.New(*cs.Umami, deps))
	}
	if cs.Stripe != nil {
		out = append(out, stripe.New(*cs.Stripe, deps))
	}
	if cs.Qonto != nil {
		out = append(out, qonto.New(*cs.Qonto, deps))
	}
	if cs.Weather != nil {
		out = append(out, weather.New(*cs.Weather, deps))
	}
	if cs.Calendar != nil {
		out = append(out, calendar.New(*cs.Calendar, deps))
	}
	if cs.Health != nil {
		out = append(out, health.New(*cs.Health, deps))
	}
	if cs.Registry != nil {
		out = append(out, registry.New(*cs.Registry, deps))
	}
	if cs.Maintenant != nil {
		out = append(out, maintenant.New(*cs.Maintenant, deps))
	}
	for _, j := range cs.JSONPoll {
		out = append(out, jsonpoll.New(j, deps))
	}
	return out
}
