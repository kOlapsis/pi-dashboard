package night

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/clock"
)

type Window struct {
	From string
	To   string
}

func parseHM(s string) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("want HH:MM, got %q", s)
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("want HH:MM, got %q", s)
	}
	return hh*60 + mm, nil
}

func (w Window) Validate() error {
	if _, err := parseHM(w.From); err != nil {
		return err
	}
	_, err := parseHM(w.To)
	return err
}

func (w Window) Active(now time.Time) bool {
	from, err1 := parseHM(w.From)
	to, err2 := parseHM(w.To)
	if err1 != nil || err2 != nil || from == to {
		return false
	}
	m := now.Hour()*60 + now.Minute()
	if from < to {
		return m >= from && m < to
	}
	return m >= from || m < to
}

// Run executes offCmd when the window opens and onCmd when it closes, checking once a minute.
func Run(ctx context.Context, clk clock.Clock, log *slog.Logger, w Window, offCmd, onCmd []string) {
	if len(offCmd) == 0 && len(onCmd) == 0 {
		return
	}
	prev := w.Active(clk.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-clk.After(time.Minute):
		}
		cur := w.Active(clk.Now())
		if cur == prev {
			continue
		}
		prev = cur
		cmd := onCmd
		if cur {
			cmd = offCmd
		}
		if len(cmd) == 0 {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		out, err := exec.CommandContext(cctx, cmd[0], cmd[1:]...).CombinedOutput()
		cancel()
		if err != nil {
			log.Warn("night command failed", "cmd", cmd, "err", err, "output", strings.TrimSpace(string(out)))
		}
	}
}
