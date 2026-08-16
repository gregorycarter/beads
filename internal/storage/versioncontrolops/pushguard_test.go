package versioncontrolops

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// resetPushGuards clears package state between tests.
func resetPushGuards(t *testing.T) {
	t.Helper()
	pushGuardMu.Lock()
	pushInFlerr = map[string]bool{}
	pushBreaker = map[string]*breakerState{}
	pushGuardMu.Unlock()
	nowFunc = time.Now
	t.Cleanup(func() { nowFunc = time.Now })
}

// A push that never returns must be abandoned rather than hanging forever.
// This is the 8.5-hour case from be-glm.
func TestGuardedPushAbandonsAfterTimeout(t *testing.T) {
	resetPushGuards(t)
	t.Setenv(envPushTimeout, "50ms")

	err := guardedPush(context.Background(), "origin", "main", func(ctx context.Context) error {
		<-ctx.Done() // simulate a push that never completes on its own
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("err = nil, want a timeout error: an unbounded push is the whole bug")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

// The accumulation, not the individual failure, is what saturates the server.
// A second push for the same remote/branch must be refused while one is running.
func TestGuardedPushRefusesConcurrentPushForSameTarget(t *testing.T) {
	resetPushGuards(t)
	t.Setenv(envPushTimeout, "5s")

	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = guardedPush(context.Background(), "origin", "main", func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	err := guardedPush(context.Background(), "origin", "main", func(context.Context) error {
		t.Error("second push ran; it must be refused while one is in flight")
		return nil
	})
	if !errors.Is(err, ErrPushInFlight) {
		t.Fatalf("err = %v, want ErrPushInFlight", err)
	}

	close(release)
	wg.Wait()
}

// One bad remote must not block a different one.
func TestGuardedPushInFlightIsPerTarget(t *testing.T) {
	resetPushGuards(t)
	t.Setenv(envPushTimeout, "5s")

	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = guardedPush(context.Background(), "origin", "main", func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	ran := false
	err := guardedPush(context.Background(), "backup", "main", func(context.Context) error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil: a different remote must not be blocked", err)
	}
	if !ran {
		t.Fatal("push to the other remote did not run")
	}

	close(release)
	wg.Wait()
}

// After repeated failures, stop attempting and say so, rather than retrying
// silently forever against an impossible target.
func TestGuardedPushOpensBreakerAfterConsecutiveFailures(t *testing.T) {
	resetPushGuards(t)
	t.Setenv(envPushTimeout, "5s")
	t.Setenv(envBreakerThreshold, "2")
	t.Setenv(envBreakerCooldown, "1h")

	boom := errors.New("remote rejected")
	for i := 0; i < 2; i++ {
		if err := guardedPush(context.Background(), "origin", "main", func(context.Context) error {
			return boom
		}); !errors.Is(err, boom) {
			t.Fatalf("attempt %d: err = %v, want the underlying error", i+1, err)
		}
	}

	err := guardedPush(context.Background(), "origin", "main", func(context.Context) error {
		t.Error("push ran while the breaker should be open")
		return nil
	})
	if !errors.Is(err, ErrPushCircuitOpen) {
		t.Fatalf("err = %v, want ErrPushCircuitOpen", err)
	}
}

// The breaker must not latch permanently — it reopens after the cooldown.
func TestGuardedPushBreakerReopensAfterCooldown(t *testing.T) {
	resetPushGuards(t)
	t.Setenv(envPushTimeout, "5s")
	t.Setenv(envBreakerThreshold, "1")
	t.Setenv(envBreakerCooldown, "10m")

	base := time.Now()
	nowFunc = func() time.Time { return base }

	boom := errors.New("remote rejected")
	_ = guardedPush(context.Background(), "origin", "main", func(context.Context) error { return boom })

	if err := guardedPush(context.Background(), "origin", "main", func(context.Context) error {
		return nil
	}); !errors.Is(err, ErrPushCircuitOpen) {
		t.Fatalf("err = %v, want the breaker open immediately after the failure", err)
	}

	nowFunc = func() time.Time { return base.Add(11 * time.Minute) }
	ran := false
	if err := guardedPush(context.Background(), "origin", "main", func(context.Context) error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("err = %v, want nil after the cooldown elapsed", err)
	}
	if !ran {
		t.Fatal("push did not run after cooldown; the breaker latched permanently")
	}
}

// A success must clear accumulated failures so a recovered remote is not
// throttled by ancient history.
func TestGuardedPushSuccessResetsBreaker(t *testing.T) {
	resetPushGuards(t)
	t.Setenv(envPushTimeout, "5s")
	t.Setenv(envBreakerThreshold, "2")

	boom := errors.New("transient")
	_ = guardedPush(context.Background(), "origin", "main", func(context.Context) error { return boom })
	_ = guardedPush(context.Background(), "origin", "main", func(context.Context) error { return nil })

	// One more failure must not trip a threshold-2 breaker if the success reset it.
	_ = guardedPush(context.Background(), "origin", "main", func(context.Context) error { return boom })

	ran := false
	if err := guardedPush(context.Background(), "origin", "main", func(context.Context) error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("err = %v, want nil: a success should have reset the failure count", err)
	}
	if !ran {
		t.Fatal("push did not run; success failed to reset the breaker")
	}
}

// A caller deadline tighter than the guard's must win.
func TestGuardedPushRespectsTighterCallerDeadline(t *testing.T) {
	resetPushGuards(t)
	t.Setenv(envPushTimeout, "1h")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := guardedPush(ctx, "origin", "main", func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if err == nil {
		t.Fatal("err = nil, want the caller deadline to apply")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("took %s; the guard ignored the tighter caller deadline", elapsed)
	}
}

// An unparseable env value must fall back to the default, never disable the guard.
func TestEnvOverridesFallBackSafely(t *testing.T) {
	t.Setenv(envPushTimeout, "not-a-duration")
	if got := envDuration(envPushTimeout, defaultPushTimeout); got != defaultPushTimeout {
		t.Fatalf("envDuration = %v, want the default %v", got, defaultPushTimeout)
	}
	t.Setenv(envPushTimeout, "-5m")
	if got := envDuration(envPushTimeout, defaultPushTimeout); got != defaultPushTimeout {
		t.Fatalf("envDuration(negative) = %v, want the default %v", got, defaultPushTimeout)
	}
	t.Setenv(envBreakerThreshold, "0")
	if got := envInt(envBreakerThreshold, defaultBreakerThreshold); got != defaultBreakerThreshold {
		t.Fatalf("envInt(0) = %v, want the default %v", got, defaultBreakerThreshold)
	}
}
