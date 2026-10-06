package clock

import (
	"sync"
	"time"
)

type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type Real struct{}

func (Real) Now() time.Time { return time.Now() }

func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }

type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []fakeTimer
}

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

func NewFake(now time.Time) *Fake { return &Fake{now: now} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if d <= 0 {
		ch <- f.now
		return ch
	}
	f.timers = append(f.timers, fakeTimer{at: f.now.Add(d), ch: ch})
	return ch
}

func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	kept := f.timers[:0]
	for _, t := range f.timers {
		if !t.at.After(f.now) {
			t.ch <- f.now
			continue
		}
		kept = append(kept, t)
	}
	f.timers = kept
}

func (f *Fake) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}
