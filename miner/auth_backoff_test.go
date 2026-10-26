package miner

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func authBackoffTestNow() time.Time { return time.Unix(1_700_000_000, 0) }

func TestAuthBackoffScheduleTiers(t *testing.T) {
	now := authBackoffTestNow()
	cases := []struct {
		count int
		age   time.Duration
		want  time.Duration
	}{
		{9, 0, 0},
		{10, 0, 60 * time.Second},
		{19, 0, 60 * time.Second},
		{20, 0, 300 * time.Second},
		{10, 30 * time.Second, 30 * time.Second},
		{10, 2 * time.Minute, 0},
	}
	for _, c := range cases {
		st := authBackoffState{ConsecutiveFailures: c.count, LastFailureUnix: now.Add(-c.age).Unix()}
		if got := authBackoffDelay(st, now); got != c.want {
			t.Errorf("count=%d age=%s: got %s want %s", c.count, c.age, got, c.want)
		}
	}
}

func TestAuthBackoffStopNeedsCountAndFloor(t *testing.T) {
	now := authBackoffTestNow()
	cases := []struct {
		count  int
		streak time.Duration
		want   bool
	}{
		{29, 3 * time.Hour, false},
		{30, time.Hour, false},
		{30, 2 * time.Hour, true},
		{31, 3 * time.Hour, true},
	}
	for _, c := range cases {
		st := authBackoffState{ConsecutiveFailures: c.count, FirstFailureUnix: now.Add(-c.streak).Unix()}
		if got := authBackoffStopped(st, now); got != c.want {
			t.Errorf("count=%d streak=%s: got %v want %v", c.count, c.streak, got, c.want)
		}
	}
}

func TestAuthBackoffCountsRejectionsAndRateLimits(t *testing.T) {
	if !authFailureCounts(&connect.HttpStatusError{StatusCode: http.StatusTooManyRequests}) {
		t.Fatal("a 429 must count toward the streak")
	}
	if authFailureCounts(&connect.HttpStatusError{StatusCode: http.StatusInternalServerError}) {
		t.Fatal("a 500 must not count")
	}
	if authFailureCounts(errors.New("connection refused")) {
		t.Fatal("a network error must not count")
	}
	if authFailureCounts(nil) {
		t.Fatal("nil must not count")
	}
}

func TestAuthBackoffStateRoundTrip(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	now := authBackoffTestNow()

	st := recordAuthFailure(now, "401")
	if st.ConsecutiveFailures != 1 || st.FirstFailureUnix != now.Unix() || st.TotalFailures != 1 {
		t.Fatalf("first failure: %+v", st)
	}
	st = recordAuthFailure(now.Add(time.Minute), "429")
	if st.ConsecutiveFailures != 2 || st.TotalFailures != 2 || st.LastFailureUnix != now.Add(time.Minute).Unix() {
		t.Fatalf("second failure: %+v", st)
	}
	previous := recordAuthSuccess(now.Add(2 * time.Minute))
	if previous.ConsecutiveFailures != 2 {
		t.Fatalf("recovery must report the streak it ended: %+v", previous)
	}
	got := readAuthBackoffState()
	if got.ConsecutiveFailures != 0 || got.FirstFailureUnix != 0 || got.LastFailureUnix != 0 {
		t.Fatalf("recovery must reset the streak: %+v", got)
	}
}

// A HotSwap candidate is a fresh process but not a fresh node. It must not
// wait, and a failed candidate must leave the parent's persisted streak alone.
func TestAuthBackoffHotSwapCandidateDoesNotWaitOrCount(t *testing.T) {
	fixture := newProviderRegistrationFixture(t)
	fixture.status = 401
	settings := fixture.settings(true)
	settings.hotSwapCandidate = true

	// The fixture owns URNETWORK_STATE_DIR, so seed after it is built.
	now := authBackoffTestNow()
	seed := authBackoffState{
		ConsecutiveFailures: 30,
		FirstFailureUnix:    now.Add(-3 * time.Hour).Unix(),
		LastFailureUnix:     now.Add(-time.Hour).Unix(),
	}
	if err := writeAuthBackoffState(seed); err != nil {
		t.Fatal(err)
	}

	previousWait := authBackoffWait
	authBackoffWait = func(ctx context.Context, d time.Duration) error {
		t.Errorf("hot-swap candidate waited %s", d)
		return nil
	}
	t.Cleanup(func() { authBackoffWait = previousWait })

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	err := settings.run(context.WithValue(ctx, providerRegistrationHooksKey{}, providerRegistrationHooks{}), &providerRefusedWriter{})
	if err == nil {
		t.Fatal("a candidate whose only slot was rejected returned nil")
	}
	if got := readAuthBackoffState(); got.ConsecutiveFailures != seed.ConsecutiveFailures {
		t.Fatalf("candidate changed the streak: %d want %d", got.ConsecutiveFailures, seed.ConsecutiveFailures)
	}
	if code, ok := providerRunExitCode(err); ok && code == minerExitAuthStopped {
		t.Fatalf("candidate asked for the stop code: %v", err)
	}
}

func TestAuthBackoffMetricsRender(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	now := time.Now()
	st := authBackoffState{
		ConsecutiveFailures: 12,
		FirstFailureUnix:    now.Add(-time.Hour).Unix(),
		// A future last-failure keeps the elapsed-time deduction from shaving
		// the wait between the write and the render.
		LastFailureUnix: now.Add(time.Hour).Unix(),
		TotalFailures:   40,
		TotalWaits:      7,
		TotalStops:      1,
		TotalRecoveries: 2,
	}
	if err := writeAuthBackoffState(st); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	writeAuthBackoffMetrics(&b)
	out := b.String()
	for _, want := range []string{
		"urnet_auth_consecutive_failures 12",
		"urnet_auth_backoff_wait_seconds 60",
		"urnet_auth_backoff_stopped 0",
		"urnet_auth_all_slots_failed_total 40",
		"urnet_auth_backoff_waits_total 7",
		"urnet_auth_backoff_stops_total 1",
		"urnet_auth_backoff_recoveries_total 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// The state file is the persistent counter store: a restart reads back the
// lifetime totals, and a recovery clears only the streak.
func TestAuthBackoffCountersSurviveRestart(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	now := time.Now()
	recordAuthFailure(now, "401")
	recordAuthFailure(now.Add(time.Minute), "429")
	st := readAuthBackoffState()
	if st.ConsecutiveFailures != 2 || st.TotalFailures != 2 {
		t.Fatalf("after two failures: %+v", st)
	}
	recordAuthSuccess(now.Add(2 * time.Minute))
	st = readAuthBackoffState()
	if st.ConsecutiveFailures != 0 || st.FirstFailureUnix != 0 {
		t.Fatalf("recovery must clear the streak: %+v", st)
	}
	if st.TotalFailures != 2 || st.TotalRecoveries != 1 {
		t.Fatalf("lifetime totals must survive the reset: %+v", st)
	}
}

// A state file written before the lifetime totals existed must still load.
func TestAuthBackoffOldStateFileLoads(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	old := `{"consecutive_auth_failures":3,"first_failure_unix":1700000000,"last_failure_unix":1700000060}`
	if err := os.WriteFile(filepath.Join(dir, authBackoffFileName), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	st := readAuthBackoffState()
	if st.ConsecutiveFailures != 3 || st.LastFailureUnix != 1700000060 {
		t.Fatalf("old file did not load: %+v", st)
	}
	if st.TotalFailures != 0 || st.TotalWaits != 0 || st.TotalStops != 0 || st.TotalRecoveries != 0 {
		t.Fatalf("new counters must default to zero: %+v", st)
	}
}
