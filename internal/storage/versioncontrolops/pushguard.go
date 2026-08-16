package versioncontrolops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

// Push guards (be-glm).
//
// A DOLT_PUSH to an unreachable or oversized remote never returns on its own.
// Observed 2026-08-16 on a live city: fifteen concurrent
// CALL DOLT_PUSH('origin','main') calls, the oldest running 8.5 hours, a new
// one starting roughly every 16 minutes, none ever completing. Each holds a
// server connection, so the pile-up — not the individual failure — saturated
// the shared Dolt server and took bd queries down fleet-wide.
//
// The push target in that case was a ~7GB database being pushed over HTTPS to
// a GitHub repo, which rejects pushes far below that size. So this was not a
// transient fault being retried; it was an impossible operation retried
// forever. Three guards, cheapest first:
//
//  1. Deadline    — a push that has run past pushTimeout is abandoned.
//  2. In-flight   — never start a second push for a remote/branch already
//     pushing. Accumulation is what actually causes the outage.
//  3. Breaker     — after consecutive failures, stop attempting for a cooldown
//     and say so, instead of retrying silently forever.
//
// All three are per (remote, branch) so one bad remote cannot stop another.

const (
	defaultPushTimeout      = 10 * time.Minute
	defaultBreakerThreshold = 3
	defaultBreakerCooldown  = 15 * time.Minute
)

// Tunables, overridable by env for operators who need to widen or disable a
// guard without a rebuild. An unparseable or non-positive value falls back to
// the default rather than silently disabling the guard.
const (
	envPushTimeout      = "BEADS_DOLT_PUSH_TIMEOUT"
	envBreakerThreshold = "BEADS_DOLT_PUSH_BREAKER_THRESHOLD"
	envBreakerCooldown  = "BEADS_DOLT_PUSH_BREAKER_COOLDOWN"
)

// ErrPushInFlight is returned when a push for the same remote/branch is already
// running. Callers should treat this as "skipped, try later", not as a failure
// of the underlying remote — retrying immediately is what caused the pile-up.
var ErrPushInFlight = errors.New("push already in flight for this remote/branch")

// ErrPushCircuitOpen is returned while the breaker is open for a
// remote/branch. It names the failure count and remaining cooldown so the
// condition is diagnosable from the error alone.
var ErrPushCircuitOpen = errors.New("push circuit breaker open")

type breakerState struct {
	consecutiveFailures int
	openUntil           time.Time
	lastErr             error
}

var (
	pushGuardMu sync.Mutex
	pushInFlerr = map[string]bool{}
	pushBreaker = map[string]*breakerState{}
)

// nowFunc is swappable in tests.
var nowFunc = time.Now

func envDuration(key string, def time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func envInt(key string, def int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func pushKey(remote, branch string) string { return remote + "\x00" + branch }

// acquirePushSlot enforces guards 2 and 3. On success the caller MUST call
// releasePushSlot exactly once with the push outcome.
func acquirePushSlot(remote, branch string) error {
	key := pushKey(remote, branch)

	pushGuardMu.Lock()
	defer pushGuardMu.Unlock()

	if pushInFlerr[key] {
		return fmt.Errorf("%w: %s/%s", ErrPushInFlight, remote, branch)
	}

	if st := pushBreaker[key]; st != nil && nowFunc().Before(st.openUntil) {
		remaining := st.openUntil.Sub(nowFunc()).Round(time.Second)
		return fmt.Errorf("%w: %s/%s after %d consecutive failures, %s remaining (last error: %v)",
			ErrPushCircuitOpen, remote, branch, st.consecutiveFailures, remaining, st.lastErr)
	}

	pushInFlerr[key] = true
	return nil
}

// releasePushSlot clears the in-flight marker and advances breaker state.
func releasePushSlot(remote, branch string, pushErr error) {
	key := pushKey(remote, branch)
	threshold := envInt(envBreakerThreshold, defaultBreakerThreshold)
	cooldown := envDuration(envBreakerCooldown, defaultBreakerCooldown)

	pushGuardMu.Lock()
	defer pushGuardMu.Unlock()

	delete(pushInFlerr, key)

	if pushErr == nil {
		// A success clears the breaker entirely: the remote is reachable again.
		delete(pushBreaker, key)
		return
	}

	st := pushBreaker[key]
	if st == nil {
		st = &breakerState{}
		pushBreaker[key] = st
	}
	st.consecutiveFailures++
	st.lastErr = pushErr
	if st.consecutiveFailures >= threshold {
		st.openUntil = nowFunc().Add(cooldown)
	}
}

// guardedPush wraps a push with all three guards. fn receives a context that
// already carries the deadline.
func guardedPush(ctx context.Context, remote, branch string, fn func(context.Context) error) error {
	if err := acquirePushSlot(remote, branch); err != nil {
		return err
	}

	timeout := envDuration(envPushTimeout, defaultPushTimeout)
	// Respect a caller deadline that is already tighter than ours.
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < timeout {
		timeout = time.Until(dl)
	}
	pushCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err := fn(pushCtx)

	// Surface a deadline as something an operator can act on. The underlying
	// driver error for an abandoned DOLT_PUSH is not self-explanatory.
	if err != nil && errors.Is(pushCtx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("push to %s/%s abandoned after %s (set %s to change): %w",
			remote, branch, timeout, envPushTimeout, err)
	}

	releasePushSlot(remote, branch, err)
	return err
}
