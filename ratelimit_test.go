package mailer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestNilLimiterIsUnlimited(t *testing.T) {
	var l *limiter // what newLimiter returns for RateLimit == 0
	if got := newLimiter(0); got != nil {
		t.Errorf("newLimiter(0) = %v, want nil", got)
	}
	if got := newLimiter(-1); got != nil {
		t.Errorf("newLimiter(-1) = %v, want nil", got)
	}

	start := time.Now()
	for range 100 {
		if err := l.wait(t.Context()); err != nil {
			t.Fatalf("wait on a nil limiter: %v", err)
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("nil limiter blocked for %v", elapsed)
	}
}

func TestLimiterPaces(t *testing.T) {
	// 100/sec: a 10ms gap between sends, so four waits after the first should
	// take at least 40ms.
	l := newLimiter(100)
	start := time.Now()
	for range 5 {
		if err := l.wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	if min := 40 * time.Millisecond; elapsed < min {
		t.Errorf("five sends took %v, want at least %v", elapsed, min)
	}
	// Generous upper bound: this asserts the pacing is not wildly wrong
	// without being flaky on a loaded machine.
	if max := 2 * time.Second; elapsed > max {
		t.Errorf("five sends took %v, far more than the %v cap", elapsed, max)
	}
}

func TestLimiterDoesNotBurst(t *testing.T) {
	// The first call is free, but a limiter idle for a while must not have
	// banked allowance to spend at once — that is what trips SES throttling.
	l := newLimiter(50) // 20ms apart
	if err := l.wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // idle, notionally "earning" 10 sends

	start := time.Now()
	for range 3 {
		if err := l.wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed < 35*time.Millisecond {
		t.Errorf("three sends after an idle period took %v; allowance was banked", elapsed)
	}
}

func TestLimiterHonoursContext(t *testing.T) {
	l := newLimiter(1) // one per second
	if err := l.wait(t.Context()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := l.wait(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("wait = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("wait ignored the deadline for %v", elapsed)
	}
}

func TestLimiterIsConcurrencySafe(t *testing.T) {
	// Run under -race: the schedule is shared mutable state.
	l := newLimiter(1000)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 5 {
				if err := l.wait(t.Context()); err != nil {
					t.Errorf("wait: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
}
