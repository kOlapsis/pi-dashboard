package main

import (
	"context"
	"flag"

	"github.com/kolapsis/pi-dashboard/internal/kiosk"
)

func runKiosk(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("kiosk", flag.ContinueOnError)
	url := fs.String("url", "http://127.0.0.1:8080/", "dashboard URL")
	output := fs.String("output", "HDMI-A-1", "Wayland output driven by wlopm")
	verbose := fs.Bool("verbose", false, "debug logging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cf := commonFlags{verbose: *verbose}
	return kiosk.Run(ctx, kiosk.Options{URL: *url, Output: *output, Log: cf.logger()})
}
