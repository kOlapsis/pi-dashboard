package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pi-dashboard:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		return runServe(ctx, args)
	case "doctor":
		return runDoctor(ctx, args)
	case "version":
		fmt.Println("pi-dashboard", version)
		return nil
	default:
		return fmt.Errorf("unknown command %q (serve, doctor, version)", cmd)
	}
}

type commonFlags struct {
	config  string
	verbose bool
}

func (f *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.config, "config", "/etc/pi-dashboard/config.yaml", "configuration file")
	fs.BoolVar(&f.verbose, "verbose", false, "debug logging")
}

func (f *commonFlags) logger() *slog.Logger {
	level := slog.LevelInfo
	if f.verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
