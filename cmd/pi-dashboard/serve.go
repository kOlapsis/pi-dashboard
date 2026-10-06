package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/collector"
	"github.com/kolapsis/pi-dashboard/internal/collector/demo"
	"github.com/kolapsis/pi-dashboard/internal/config"
	"github.com/kolapsis/pi-dashboard/internal/httpapi"
	"github.com/kolapsis/pi-dashboard/internal/httpx"
	"github.com/kolapsis/pi-dashboard/internal/night"
	"github.com/kolapsis/pi-dashboard/internal/sched"
	"github.com/kolapsis/pi-dashboard/internal/store"
	"github.com/kolapsis/pi-dashboard/internal/web"
)

const snapshotFile = "snapshot.json"

func runServe(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	var cf commonFlags
	cf.register(flags)
	addr := flags.String("addr", "", "listen address (overrides config)")
	dataDir := flags.String("data", "", "data directory (overrides config)")
	demoMode := flags.Bool("demo", false, "serve generated data, no config or credentials needed")
	demoFail := flags.String("demo-fail", "", "comma-separated demo collectors that always fail")
	demoStale := flags.String("demo-stale", "", "comma-separated demo collectors that succeed once, then fail")
	staticDir := flags.String("static-dir", "", "serve the web UI from this directory instead of the embedded copy")
	if err := flags.Parse(args); err != nil {
		return err
	}
	log := cf.logger()
	static := web.FS()
	if *staticDir != "" {
		static = os.DirFS(*staticDir)
	}

	var (
		cfg      *config.Config
		warnings []string
		err      error
	)
	if *demoMode {
		cfg = config.Default()
	} else {
		cfg, warnings, err = config.Load(cf.config)
		if err != nil {
			return err
		}
	}
	for _, w := range warnings {
		log.Warn(w)
	}
	if *addr != "" {
		cfg.Listen = *addr
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return err
	}
	clk := clock.Real{}

	var hist store.History
	var db *store.DB
	if *demoMode {
		hist = store.NewMem()
	} else {
		db, err = store.Open(filepath.Join(cfg.DataDir, "history.db"))
		if err != nil {
			return fmt.Errorf("open history: %w", err)
		}
		defer func() { _ = db.Close() }()
		hist = db
	}

	deps := collector.Deps{HTTP: httpx.NewClient(), Clock: clk, Hist: hist, Log: log, Loc: loc}
	s := sched.New(clk, log, sched.Options{})
	if *demoMode {
		modes := map[string]demo.Mode{}
		for _, n := range splitList(*demoFail) {
			modes[n] = "fail"
		}
		for _, n := range splitList(*demoStale) {
			modes[n] = "stale"
		}
		s.Add(demo.Collectors(clk, loc, 42, modes)...)
	} else {
		s.Add(buildCollectors(cfg, deps)...)
		if prev, err := loadSnapshot(filepath.Join(cfg.DataDir, snapshotFile)); err != nil {
			log.Warn("snapshot load failed", "err", err)
		} else if prev != nil {
			s.Restore(*prev)
		}
	}

	srv := &httpapi.Server{
		Sched:   s,
		Static:  static,
		Clock:   clk,
		Log:     log,
		Version: version,
		TZ:      cfg.Timezone,
		Demo:    *demoMode,
		UI: httpapi.UI{
			Scale: cfg.UI.Scale,
			Night: httpapi.Night{Enabled: cfg.UI.Night.On(), From: cfg.UI.Night.From, To: cfg.UI.Night.To, Brightness: cfg.UI.Night.Brightness},
		},
	}
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	log.Info("listening", "addr", ln.Addr().String(), "version", version, "demo", *demoMode, "collectors", len(s.Collectors()))

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return s.Run(gctx) })
	g.Go(func() error {
		err := httpSrv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	})
	g.Go(func() error {
		<-gctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	})
	if !*demoMode {
		g.Go(func() error {
			persistSnapshots(gctx, clk, log, s, filepath.Join(cfg.DataDir, snapshotFile))
			return nil
		})
		g.Go(func() error {
			pruneHistory(gctx, clk, log, db, cfg.History.RetentionDays)
			return nil
		})
	}
	if cfg.UI.Night.On() {
		g.Go(func() error {
			night.Run(gctx, clk, log, night.Window{From: cfg.UI.Night.From, To: cfg.UI.Night.To}, cfg.UI.Night.PanelOffCmd, cfg.UI.Night.PanelOnCmd)
			return nil
		})
	}
	err = g.Wait()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func loadSnapshot(path string) (*sched.Snapshot, error) {
	b, err := store.LoadFile(path)
	if err != nil || b == nil {
		return nil, err
	}
	var snap sched.Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

func persistSnapshots(ctx context.Context, clk clock.Clock, log *slog.Logger, s *sched.Scheduler, path string) {
	ch, cancel := s.Subscribe()
	defer cancel()
	dirty := false
	for {
		select {
		case <-ctx.Done():
			if dirty {
				saveSnapshot(log, s, path)
			}
			return
		case <-ch:
			dirty = true
		case <-clk.After(30 * time.Second):
			if dirty {
				saveSnapshot(log, s, path)
				dirty = false
			}
		}
	}
}

func saveSnapshot(log *slog.Logger, s *sched.Scheduler, path string) {
	b, err := json.Marshal(s.Snapshot())
	if err == nil {
		err = store.SaveFile(path, b)
	}
	if err != nil {
		log.Warn("snapshot save failed", "err", err)
	}
}

func pruneHistory(ctx context.Context, clk clock.Clock, log *slog.Logger, db *store.DB, retentionDays int) {
	for {
		n, err := db.Prune(ctx, clk.Now().AddDate(0, 0, -retentionDays))
		if err != nil {
			log.Warn("history prune failed", "err", err)
		} else if n > 0 {
			log.Info("history pruned", "rows", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-clk.After(24 * time.Hour):
		}
	}
}
