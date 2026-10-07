package kiosk

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
)

type ExecRunner struct {
	Log *slog.Logger
}

func (r ExecRunner) Run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil && r.Log != nil {
		r.Log.Warn("command failed", "cmd", name, "args", args, "output", strings.TrimSpace(string(out)))
	}
	return err
}

func (r ExecRunner) Start(ctx context.Context, name string, args ...string) (func(), error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := cmd.Wait(); err != nil && ctx.Err() == nil && r.Log != nil {
			r.Log.Warn("command exited", "cmd", name, "err", err)
		}
	}()
	return func() {
		_ = cmd.Process.Kill()
		<-done
	}, nil
}
