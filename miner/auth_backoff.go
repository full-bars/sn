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
	"time"

	"github.com/urnetwork/connect"
	"github.com/urfoundation/sn/clientauth"
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

// authBackoffState is the persisted streak. The file is the source of truth;
// the metrics counters are separate and monotonic.
type authBackoffState struct {
	ConsecutiveFailures int    `json:"consecutive_auth_failures"`
	FirstFailureUnix    int64  `json:"first_failure_unix"`
	LastFailureUnix     int64  `json:"last_failure_unix"`
	Stopped             bool   `json:"stopped"`
	StoppedAtUnix       int64  `json:"stopped_at_unix"`
	TotalFailures       int64  `json:"total_failures_lifetime"`
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
	previous = readAuthBackoffState()
	if previous.ConsecutiveFailures == 0 {
		return previous
	}
	st := previous
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

func logAuthBackoffBanner(st authBackoffState, wait time.Duration, now time.Time) {
	streak := time.Duration(0)
	if st.FirstFailureUnix > 0 {
		streak = now.Sub(time.Unix(st.FirstFailureUnix, 0))
	}
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
	streak := time.Duration(0)
	if st.FirstFailureUnix > 0 {
		streak = now.Sub(time.Unix(st.FirstFailureUnix, 0))
	}
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
	streak := time.Duration(0)
	if previous.FirstFailureUnix > 0 {
		streak = now.Sub(time.Unix(previous.FirstFailureUnix, 0))
	}
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
	logAuthBackoffBanner(st, wait, now)
	if wait > 0 {
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
	_ = writeAuthBackoffState(st)
	logAuthStopBanner(st, now)
	return errProviderAuthStopped
}
