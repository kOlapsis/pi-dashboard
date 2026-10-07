package httpapi

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kolapsis/pi-dashboard/internal/clock"
	"github.com/kolapsis/pi-dashboard/internal/sched"
)

const keepAlive = 25 * time.Second

type UI struct {
	Scale string `json:"scale"`
	Idle  Idle   `json:"idle"`
	Night Night  `json:"night"`
}

type Idle struct {
	TimeoutS int `json:"timeout_s"`
}

type Night struct {
	Enabled    bool    `json:"enabled"`
	From       string  `json:"from"`
	To         string  `json:"to"`
	Brightness float64 `json:"brightness"`
	ScreenOff  bool    `json:"screen_off"`
}

type State struct {
	Seq        uint64                 `json:"seq"`
	Version    string                 `json:"version"`
	Now        time.Time              `json:"now"`
	TZ         string                 `json:"tz"`
	Demo       bool                   `json:"demo"`
	UI         UI                     `json:"ui"`
	Collectors map[string]sched.Entry `json:"collectors"`
	Events     []sched.Event          `json:"events"`
}

type Server struct {
	Sched   *sched.Scheduler
	Static  fs.FS
	Clock   clock.Clock
	Log     *slog.Logger
	Version string
	TZ      string
	Demo    bool
	UI      UI
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /", s.static())
	return secure(mux)
}

func (s *Server) state(snap sched.Snapshot) State {
	return State{
		Seq:        snap.Seq,
		Version:    s.Version,
		Now:        snap.Now,
		TZ:         s.TZ,
		Demo:       s.Demo,
		UI:         s.UI,
		Collectors: snap.Collectors,
		Events:     snap.Events,
	}
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(s.state(s.Sched.Snapshot())); err != nil {
		s.Log.Warn("state encode", "err", err)
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(snap sched.Snapshot) bool {
		b, err := json.Marshal(s.state(snap))
		if err != nil {
			s.Log.Warn("event encode", "err", err)
			return true
		}
		if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	_, _ = fmt.Fprint(w, "retry: 3000\n\n")
	if !send(s.Sched.Snapshot()) {
		return
	}
	ch, cancel := s.Sched.Subscribe()
	defer cancel()
	ticker := time.NewTicker(keepAlive)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case snap := <-ch:
			if !send(snap) {
				return
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) static() http.Handler {
	files := http.FileServerFS(s.Static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/" || r.URL.Path == "/index.html":
			w.Header().Set("Cache-Control", "no-store")
		case strings.HasPrefix(r.URL.Path, "/fonts/"):
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		default:
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
