package mailer

import (
	"context"
	"sync"
	"time"
)

// limiter paces sends to a fixed number per second.
//
// It is a strict pacer, not a token bucket: it spaces messages evenly and
// never allows a burst. That is the right shape for SES, which enforces a
// per-second rate — saving up allowance and then spending it in one burst just
// buys a "454 Throttling failure" and, repeated, a reputation problem. Being
// slightly slower than the cap is the safe direction to be wrong in.
//
// A nil *limiter is a working no-op, so the unlimited case needs no branch at
// the call site.
type limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

// newLimiter returns nil when perSecond is zero or negative, meaning no limit.
func newLimiter(perSecond float64) *limiter {
	if perSecond <= 0 {
		return nil
	}
	return &limiter{interval: time.Duration(float64(time.Second) / perSecond)}
}

// wait blocks until the caller's turn, or until ctx is done.
func (l *limiter) wait(ctx context.Context) error {
	if l == nil {
		return ctx.Err()
	}

	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	at := l.next
	l.next = at.Add(l.interval)
	l.mu.Unlock()

	// A cancelled caller forfeits its slot rather than releasing it back. That
	// leaves a gap in the schedule, which errs towards sending too slowly.
	d := time.Until(at)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
