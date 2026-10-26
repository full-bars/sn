package miner

// auth_backoff.go keeps a persistent streak of authentication failures so a
// node whose credentials are wrong backs off instead of asking the sign-in
// endpoint for a new session every RestartSec.
//
// The server allows five failed attempts per five minutes per address and 300
// per thirty minutes, refusing with 429 (urnetwork/server
// model/auth_model_attempt.go: AttemptLookback, AttemptFailedCountThreshold).
// The schedule below stays under that while the streak is small and then
// widens to one attempt per five minutes. A rate-limit refusal counts as a
// failure: without it the counter would stop at five and the node would keep
// retrying into the same 429.
//
// The streak is not a park by itself. Only after 30 refusals spanning at
// least two hours does the run ask for the terminal exit code, so a fast
// burst cannot stop a node that might recover on the next attempt.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/urfoundation/sn/clientauth"
	"github.com/urnetwork/connect"
)

const (
	authBackoffFileName = "auth_backoff.json"

	// Tiers are consecutive counted failures, not elapsed time.
	authBackoffMediumFailures = 10
	authBackoffSlowFailures   = 20
	authBackoffStopFailures   = 30

	authBackoffMediumWait = 60 * time.Second
	authBackoffSlowWait   = 300 * time.Second

	// authBackoffStopFloor keeps a fast burst from parking a node. With the
	// schedule above the thirtieth failure cannot arrive in under roughly
	// 100 minutes, so the floor is the two-hour mark.
	authBackoffStopFloor = 2 * time.Hour

	authBackoffReminderEvery = 5 * time.Minute
)

// authBackoffMu serializes the streak file's read-modify-write. Every slot of a
// start authenticates at once, so without it each of them reads the same
// pre-reset state, logs its own recovery banner and adds one to the recovery
// count.
var authBackoffMu sync.Mutex

// authBackoffState is the persisted streak. The file is the source of truth;
// the metrics counters are separate and monotonic.
type authBackoffState struct {
	ConsecutiveFailures int    `json:"consecutive_auth_failures"`
	FirstFailureUnix    int64  `json:"first_failure_unix"`
	LastFailureUnix     int64  `json:"last_failure_unix"`
	Stopped             bool   `json:"stopped"`
	StoppedAtUnix       int64  `json:"stopped_at_unix"`
	TotalFailures       int64  `json:"total_failures_lifetime"`
	TotalWaits          int64  `json:"total_waits_lifetime"`
	TotalStops          int64  `json:"total_stops_lifetime"`
	TotalRecoveries     int64  `json:"total_recoveries_lifetime"`
	LastReason          string `json:"last_reason"`
}

// Injected so tests need no sleeps and no wall clock.
var (
	authBackoffNow  = time.Now
	authBackoffWait = func(ctx context.Context, d time.Duration) error {
		if d <= 0 {
			return nil
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
)

func authBackoffPath() (string, error) {
	dir, err := providerStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, authBackoffFileName), nil
}

func readAuthBackoffState() authBackoffState {
	var st authBackoffState
	if path, err := authBackoffPath(); err == nil {
		oomReadJSON(path, &st)
	}
	return st
}

// writeAuthBackoffState is temp + fsync + rename, under the same state-file
// lock oomWriteJSON uses so a HotSwap parent and candidate cannot interleave.
func writeAuthBackoffState(st authBackoffState) error {
	path, err := authBackoffPath()
	if err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	release, err := acquireJWTStoreLock(path)
	if err != nil {
		return err
	}
	defer release()
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// authBackoffDelay is how long the next start should wait before its first
// login. The time already spent restarting counts against the wait so a slow
// restart does not wait twice.
func authBackoffDelay(st authBackoffState, now time.Time) time.Duration {
	var d time.Duration
	switch {
	case st.ConsecutiveFailures >= authBackoffSlowFailures:
		d = authBackoffSlowWait
	case st.ConsecutiveFailures >= authBackoffMediumFailures:
		d = authBackoffMediumWait
	default:
		return 0
	}
	if st.LastFailureUnix > 0 {
		if elapsed := now.Sub(time.Unix(st.LastFailureUnix, 0)); elapsed > 0 {
			d -= elapsed
		}
	}
	if d < 0 {
		return 0
	}
	return d
}

// authBackoffStopped is the terminal condition: enough counted failures AND a
// streak old enough that the node has had a real chance to recover.
func authBackoffStopped(st authBackoffState, now time.Time) bool {
	if st.ConsecutiveFailures < authBackoffStopFailures || st.FirstFailureUnix <= 0 {
		return false
	}
	return now.Sub(time.Unix(st.FirstFailureUnix, 0)) >= authBackoffStopFloor
}

// authFailureCounts separates a refused login from a network or protocol
// failure. A 401 is a credential rejection; a 429 is the server's own
// rate-limit refusal, which is still a failed login and must advance the
// streak or the backoff stops engaging after the first five attempts.
func authFailureCounts(err error) bool {
	if err == nil {
		return false
	}
	if clientauth.IsNetworkCredentialRejected(err) {
		return true
	}
	var status *connect.HttpStatusError
	return errors.As(err, &status) && status.StatusCode == http.StatusTooManyRequests
}

func allAuthFailuresCount(results []error) bool {
	if len(results) == 0 {
		return false
	}
	for _, err := range results {
		if !authFailureCounts(err) {
			return false
		}
	}
	return true
}

func recordAuthFailure(now time.Time, reason string) authBackoffState {
	authBackoffMu.Lock()
	defer authBackoffMu.Unlock()
	st := readAuthBackoffState()
	st.ConsecutiveFailures++
	st.TotalFailures++
	if st.FirstFailureUnix == 0 {
		st.FirstFailureUnix = now.Unix()
	}
	st.LastFailureUnix = now.Unix()
	if len(reason) > 200 {
		reason = reason[:200]
	}
	st.LastReason = reason
	_ = writeAuthBackoffState(st)
	return st
}

// recordAuthSuccess resets the streak and returns the state as it was before
// the reset, so the caller can report what the recovery ended. It never
// increments anything: a running provider that authenticates is the healthy
// path and must not touch the failure counter.
func recordAuthSuccess(now time.Time) (previous authBackoffState) {
	authBackoffMu.Lock()
	defer authBackoffMu.Unlock()
	previous = readAuthBackoffState()
	if previous.ConsecutiveFailures == 0 {
		return previous
	}
	st := previous
	st.TotalRecoveries++
	st.ConsecutiveFailures = 0
	st.FirstFailureUnix = 0
	st.LastFailureUnix = 0
	st.Stopped = false
	st.StoppedAtUnix = 0
	st.LastReason = ""
	_ = writeAuthBackoffState(st)
	return previous
}

func authBanner(title string, lines ...string) string {
	rule := strings.Repeat("!", 72)
	var b strings.Builder
	b.WriteString(rule + "\n")
	b.WriteString("!! " + title + "\n")
	for _, line := range lines {
		b.WriteString("!! " + line + "\n")
	}
	b.WriteString(rule + "\n")
	return b.String()
}

// authStreakSince is the streak length, clamped at zero: a clock that moved
// backwards must not print a negative duration.
func authStreakSince(firstUnix int64, now time.Time) time.Duration {
	if firstUnix <= 0 {
		return 0
	}
	if streak := now.Sub(time.Unix(firstUnix, 0)); streak > 0 {
		return streak
	}
	return 0
}

func logAuthBackoffBanner(st authBackoffState, wait time.Duration, now time.Time) {
	streak := authStreakSince(st.FirstFailureUnix, now)
	banner := authBanner("AUTH FAILURE BACKOFF ACTIVE",
		fmt.Sprintf("consecutive failures: %d", st.ConsecutiveFailures),
		fmt.Sprintf("streak: %s since the first failure", streak.Round(time.Second)),
		fmt.Sprintf("wait before the next login: %s", wait.Round(time.Second)),
		fmt.Sprintf("stop threshold: %d failures and %s", authBackoffStopFailures, authBackoffStopFloor),
		"check the credentials; restart after fixing them",
	)
	tlog("%s", banner)
	critLog("%s", banner)
}

func logAuthBackoffTier(st authBackoffState, wait time.Duration) {
	line := authBanner("AUTH BACKOFF SLOWED",
		fmt.Sprintf("consecutive failures: %d", st.ConsecutiveFailures),
		fmt.Sprintf("next login waits: %s", wait.Round(time.Second)),
		"the schedule widens at 10 and 20 failures to stay under the server rate limit",
	)
	tlog("%s", line)
	critLog("%s", line)
}

func logAuthStopBanner(st authBackoffState, now time.Time) {
	streak := authStreakSince(st.FirstFailureUnix, now)
	banner := authBanner("AUTHENTICATION STOPPED",
		fmt.Sprintf("credentials rejected %d times over %s", st.ConsecutiveFailures, streak.Round(time.Minute)),
		fmt.Sprintf("node stopped with exit code %d (minerExitAuthStopped)", minerExitAuthStopped),
		"fix the credentials, then start the node again",
	)
	tlog("%s", banner)
	critLog("%s", banner)
	notifySystemdStatus(fmt.Sprintf("authentication stopped after %d failures over %s; fix the credentials and restart",
		st.ConsecutiveFailures, streak.Round(time.Minute)))
}

func logAuthRecoveredBanner(previous authBackoffState, now time.Time) {
	streak := authStreakSince(previous.FirstFailureUnix, now)
	banner := authBanner("AUTHENTICATION RECOVERED",
		fmt.Sprintf("recovered after %d consecutive failures over %s", previous.ConsecutiveFailures, streak.Round(time.Minute)),
		"the failure streak is reset",
	)
	tlog("%s", banner)
	critLog("%s", banner)
}

// runAuthBackoffWait waits before the first login of a start when the streak
// is already deep enough. READY is announced earlier in run
// (announceStartupComplete, before this call), and the liveness watchdog is
// already running, so a long wait neither trips the Type=notify start timeout
// nor starves WATCHDOG=1.
func runAuthBackoffWait(ctx context.Context) error {
	now := authBackoffNow()
	st := readAuthBackoffState()
	if st.ConsecutiveFailures == 0 {
		return nil
	}
	wait := authBackoffDelay(st, now)
	if wait > 0 {
		logAuthBackoffBanner(st, wait, now)
		st.TotalWaits++
		_ = writeAuthBackoffState(st)
		notifySystemdStatus(fmt.Sprintf("auth backoff, %d failures, next login in %s",
			st.ConsecutiveFailures, wait.Round(time.Second)))
		tlog("[auth-backoff] %d consecutive failures, next login in %s\n",
			st.ConsecutiveFailures, wait.Round(time.Second))
		remaining := wait
		for remaining > 0 {
			chunk := remaining
			if chunk > authBackoffReminderEvery {
				chunk = authBackoffReminderEvery
			}
			if err := authBackoffWait(ctx, chunk); err != nil {
				return err
			}
			remaining -= chunk
			if remaining > 0 {
				tlog("[auth-backoff] still waiting, %s remaining\n", remaining.Round(time.Second))
			}
		}
	}
	return nil
}

// authBackoffTerminal is the stop decision after an all-slots-failed run. It
// records the failure and returns the stop sentinel once the streak and floor
// are both met.
func authBackoffTerminal(now time.Time, reason string) error {
	st := recordAuthFailure(now, reason)
	if !authBackoffStopped(st, now) {
		if wait := authBackoffDelay(st, now); wait > 0 &&
			(st.ConsecutiveFailures == authBackoffMediumFailures || st.ConsecutiveFailures == authBackoffSlowFailures) {
			logAuthBackoffTier(st, wait)
		}
		return nil
	}
	st.Stopped = true
	st.StoppedAtUnix = now.Unix()
	st.TotalStops++
	_ = writeAuthBackoffState(st)
	logAuthStopBanner(st, now)
	streak := time.Duration(0)
	if st.FirstFailureUnix > 0 {
		streak = now.Sub(time.Unix(st.FirstFailureUnix, 0))
	}
	captureIncidentBundle(incidentTriggerAuthStop,
		fmt.Sprintf("authentication stopped after %d failures over %s", st.ConsecutiveFailures, streak.Round(time.Minute)), nil)
	return errProviderAuthStopped
}

// writeAuthBackoffMetrics renders the streak and its lifetime counters. The
// state file is the persistent store: every transition already rewrites it, so
// the counters survive a restart without a second file. The counters only go
// up; the streak gauges fall back to 0 on recovery.
func writeAuthBackoffMetrics(b *strings.Builder) {
	st := readAuthBackoffState()
	now := authBackoffNow()
	wait := authBackoffDelay(st, now)
	streak := 0.0
	if st.FirstFailureUnix > 0 {
		streak = now.Sub(time.Unix(st.FirstFailureUnix, 0)).Seconds()
		if streak < 0 {
			streak = 0
		}
	}
	stopped := 0
	if st.Stopped {
		stopped = 1
	}
	fmt.Fprintf(b, "# HELP urnet_auth_consecutive_failures Counted sign-in refusals in the current streak (401 or 429).\n")
	fmt.Fprintf(b, "# TYPE urnet_auth_consecutive_failures gauge\n")
	fmt.Fprintf(b, "urnet_auth_consecutive_failures %d\n", st.ConsecutiveFailures)
	fmt.Fprintf(b, "# HELP urnet_auth_backoff_wait_seconds Wait before the next sign-in; 0 when idle.\n")
	fmt.Fprintf(b, "# TYPE urnet_auth_backoff_wait_seconds gauge\n")
	fmt.Fprintf(b, "urnet_auth_backoff_wait_seconds %.0f\n", wait.Seconds())
	fmt.Fprintf(b, "# HELP urnet_auth_backoff_streak_seconds Age of the current failure streak; 0 when idle.\n")
	fmt.Fprintf(b, "# TYPE urnet_auth_backoff_streak_seconds gauge\n")
	fmt.Fprintf(b, "urnet_auth_backoff_streak_seconds %.0f\n", streak)
	fmt.Fprintf(b, "# HELP urnet_auth_backoff_stopped 1 once the node has decided to stop after repeated refusals.\n")
	fmt.Fprintf(b, "# TYPE urnet_auth_backoff_stopped gauge\n")
	fmt.Fprintf(b, "urnet_auth_backoff_stopped %d\n", stopped)
	fmt.Fprintf(b, "# HELP urnet_auth_all_slots_failed_total Lifetime runs where every slot was refused for its credentials.\n")
	fmt.Fprintf(b, "# TYPE urnet_auth_all_slots_failed_total counter\n")
	fmt.Fprintf(b, "urnet_auth_all_slots_failed_total %d\n", st.TotalFailures)
	fmt.Fprintf(b, "# HELP urnet_auth_backoff_waits_total Lifetime backoff waits before a first sign-in.\n")
	fmt.Fprintf(b, "# TYPE urnet_auth_backoff_waits_total counter\n")
	fmt.Fprintf(b, "urnet_auth_backoff_waits_total %d\n", st.TotalWaits)
	fmt.Fprintf(b, "# HELP urnet_auth_backoff_stops_total Lifetime terminal stops after repeated refusals.\n")
	fmt.Fprintf(b, "# TYPE urnet_auth_backoff_stops_total counter\n")
	fmt.Fprintf(b, "urnet_auth_backoff_stops_total %d\n", st.TotalStops)
	fmt.Fprintf(b, "# HELP urnet_auth_backoff_recoveries_total Lifetime recoveries from a non-zero failure streak.\n")
	fmt.Fprintf(b, "# TYPE urnet_auth_backoff_recoveries_total counter\n")
	fmt.Fprintf(b, "urnet_auth_backoff_recoveries_total %d\n", st.TotalRecoveries)
}
