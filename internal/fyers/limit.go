package fyers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"serene/internal/market"
)

// Limits are a plan's request caps. Fyers blocks an account for the rest of the
// day once it breaks the per-minute cap more than three times, so the client
// stays under every cap rather than retrying into one.
type Limits struct {
	Name      string
	PerSecond int
	PerMinute int
	PerDay    int
	// ExpiredIntraday caps intraday requests for expired contracts per day.
	// Zero means only the overall caps apply.
	ExpiredIntraday int
}

// Prime and Standard are Fyers' published caps, less a margin for requests
// made elsewhere with the same app, such as the Fyers web terminal.
var (
	Prime    = Limits{Name: "prime", PerSecond: 9, PerMinute: 450, PerDay: 480_000}
	Standard = Limits{Name: "standard", PerSecond: 4, PerMinute: 45, PerDay: 4_800, ExpiredIntraday: 10}
)

// LimitsFor returns a plan's limits by name.
func LimitsFor(plan string) (Limits, error) {
	switch plan {
	case "", "prime":
		return Prime, nil
	case "standard":
		return Standard, nil
	}
	return Limits{}, fmt.Errorf("unknown Fyers plan %q: use prime or standard", plan)
}

// ErrBudget means a daily cap is used up. The job should stop and resume the
// next day; nothing is lost.
var ErrBudget = errors.New("fyers daily request budget is used up: resume tomorrow")

// Usage is one IST day's request counts, kept on disk so separate runs on the
// same day share the daily caps.
type Usage struct {
	Date            string `json:"date"`
	Requests        int    `json:"requests"`
	ExpiredIntraday int    `json:"expired_intraday"`
}

// Limiter paces requests across goroutines.
type Limiter struct {
	limits Limits
	path   string // usage file; empty keeps usage in memory only
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error

	mu     sync.Mutex
	recent []time.Time // request times within the last minute
	usage  Usage
}

// NewLimiter returns a limiter that records daily usage in path.
func NewLimiter(l Limits, path string) (*Limiter, error) {
	lim := &Limiter{limits: l, path: path, now: time.Now, sleep: sleep}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil {
			if err := json.Unmarshal(b, &lim.usage); err != nil {
				return nil, fmt.Errorf("read %s: %w", path, err)
			}
		}
	}
	return lim, nil
}

// Limits returns the plan's caps.
func (l *Limiter) Limits() Limits { return l.limits }

// Usage returns today's counts.
func (l *Limiter) Usage() Usage {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollDay()
	return l.usage
}

func (l *Limiter) rollDay() {
	today := market.DateOf(l.now()).String()
	if l.usage.Date != today {
		l.usage = Usage{Date: today}
	}
}

// Wait blocks until a request may be sent, then counts it. expiredIntraday
// marks a request against the expired-contract intraday cap.
func (l *Limiter) Wait(ctx context.Context, expiredIntraday bool) error {
	for {
		l.mu.Lock()
		l.rollDay()
		if l.usage.Requests >= l.limits.PerDay {
			l.mu.Unlock()
			return ErrBudget
		}
		if expiredIntraday && l.limits.ExpiredIntraday > 0 && l.usage.ExpiredIntraday >= l.limits.ExpiredIntraday {
			l.mu.Unlock()
			return fmt.Errorf("%w (expired-contract intraday cap of %d)", ErrBudget, l.limits.ExpiredIntraday)
		}
		now := l.now()
		keep := l.recent[:0]
		for _, t := range l.recent {
			if now.Sub(t) < time.Minute {
				keep = append(keep, t)
			}
		}
		l.recent = keep
		wait := time.Duration(0)
		if len(l.recent) >= l.limits.PerMinute {
			wait = l.recent[len(l.recent)-l.limits.PerMinute].Add(time.Minute).Sub(now)
		}
		if n := len(l.recent); n >= l.limits.PerSecond {
			if w := l.recent[n-l.limits.PerSecond].Add(time.Second).Sub(now); w > wait {
				wait = w
			}
		}
		if wait <= 0 {
			l.recent = append(l.recent, now)
			l.usage.Requests++
			if expiredIntraday {
				l.usage.ExpiredIntraday++
			}
			err := l.save()
			l.mu.Unlock()
			return err
		}
		l.mu.Unlock()
		if err := l.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// save writes the usage file. It runs on every request, which is cheap next to
// a network call and means a crash can't undercount the day.
func (l *Limiter) save() error {
	if l.path == "" {
		return nil
	}
	b, err := json.Marshal(l.usage)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
