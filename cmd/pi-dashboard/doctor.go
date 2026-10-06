package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/httpx"
	"github.com/kolapsis/pi-dashboard/internal/store"
)

func runDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	var cf commonFlags
	cf.register(fs)
	only := fs.String("only", "", "comma-separated collector names to test")
	asJSON := fs.Bool("json", false, "print the collected data as JSON")
	timeout := fs.Duration("timeout", 30*time.Second, "per-collector timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, warnings, err := config.Load(cf.config)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return err
	}
	deps := collector.Deps{HTTP: httpx.NewClient(), Clock: clock.Real{}, Hist: store.NewMem(), Log: cf.logger(), Loc: loc}
	filter := splitList(*only)
	failed := 0
	tested := 0
	for _, c := range buildCollectors(cfg, deps) {
		if len(filter) > 0 && !slices.Contains(filter, c.Name()) {
			continue
		}
		tested++
		start := time.Now()
		cctx, cancel := context.WithTimeout(ctx, *timeout)
		data, err := c.Collect(cctx)
		cancel()
		dur := time.Since(start).Round(time.Millisecond)
		if err != nil {
			failed++
			fmt.Printf("KO  %-12s %v (%s)\n", c.Name(), err, dur)
			continue
		}
		summary := ""
		if s, ok := c.(collector.Summarizer); ok {
			summary = s.Summary(data)
		}
		fmt.Printf("OK  %-12s %s (%s)\n", c.Name(), summary, dur)
		if *asJSON {
			b, _ := json.MarshalIndent(data, "    ", "  ")
			fmt.Printf("    %s\n", b)
		}
	}
	if tested == 0 {
		return errors.New("no collector matched")
	}
	if failed > 0 {
		return fmt.Errorf("%d/%d collectors failed", failed, tested)
	}
	return nil
}
