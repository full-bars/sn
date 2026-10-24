package urnettools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseIdleArgs(t *testing.T) {
	// Defaults
	opts, rest, err := parseIdleArgs([]string{"--unit=urnetwork.service", "--tag=v3.23.0-fix.30.9"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Threshold != 5120 {
		t.Errorf("expected default threshold 5120, got %d", opts.Threshold)
	}
	if opts.Window != 5*time.Minute {
		t.Errorf("expected default window 5m, got %v", opts.Window)
	}
	if opts.Timeout != 30*time.Minute {
		t.Errorf("expected default timeout 30m, got %v", opts.Timeout)
	}
	if len(rest) != 2 {
		t.Errorf("expected 2 remaining args, got %v", rest)
	}

	// Custom values
	opts, rest, err = parseIdleArgs([]string{
		"--threshold", "10240",
		"--window", "10s",
		"--timeout", "1m",
		"--tag", "v3.23.0-fix.30.9",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Threshold != 10240 {
		t.Errorf("expected threshold 10240, got %d", opts.Threshold)
	}
	if opts.Window != 10*time.Second {
		t.Errorf("expected window 10s, got %v", opts.Window)
	}
	if opts.Timeout != 1*time.Minute {
		t.Errorf("expected timeout 1m, got %v", opts.Timeout)
	}
	if len(rest) != 2 {
		t.Errorf("expected 2 remaining args, got %v", rest)
	}

	// Equals syntax
	opts, rest, err = parseIdleArgs([]string{
		"--threshold=2048",
		"--window=60",
		"--timeout=300",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.Threshold != 2048 {
		t.Errorf("expected threshold 2048, got %d", opts.Threshold)
	}
	if opts.Window != 60*time.Second {
		t.Errorf("expected window 60s, got %v", opts.Window)
	}
	if opts.Timeout != 300*time.Second {
		t.Errorf("expected timeout 300s, got %v", opts.Timeout)
	}
}

func TestReadBillableRateFile(t *testing.T) {
	dir := t.TempDir()
	ratePath := filepath.Join(dir, "billable_rate")

	// Not found
	rate, found, err := readBillableRateFile(dir)
	if err != nil || found || rate != 0 {
		t.Fatalf("expected not found, got rate=%d, found=%v, err=%v", rate, found, err)
	}

	// Valid rate
	if err := os.WriteFile(ratePath, []byte("12345\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rate, found, err = readBillableRateFile(dir)
	if err != nil || !found || rate != 12345 {
		t.Fatalf("expected rate=12345, got rate=%d, found=%v, err=%v", rate, found, err)
	}

	// Malformed rate
	if err := os.WriteFile(ratePath, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _, err = readBillableRateFile(dir)
	if err == nil {
		t.Fatalf("expected error on malformed rate file, got nil")
	}
}

func TestWaitForIdleImmediate(t *testing.T) {
	ctx := context.Background()
	opts := IdleUpdateOptions{Window: 0}
	err := waitForIdle(ctx, "/tmp", true, opts, nil)
	if err != nil {
		t.Fatalf("expected immediate completion on window=0, got %v", err)
	}
}

func TestWaitForIdleStoppedProvider(t *testing.T) {
	ctx := context.Background()
	opts := IdleUpdateOptions{Window: 1 * time.Second, Timeout: 5 * time.Second}
	// When provider is stopped and billable_rate is missing, traffic is 0
	pollFn := func() (uint64, bool, error) {
		return 0, false, nil
	}
	err := waitForIdle(ctx, "/tmp", false, opts, pollFn)
	if err != nil {
		t.Fatalf("expected completion for stopped provider, got %v", err)
	}
}

// TestWaitForIdleStoppedProviderWithStaleRate pins the stopped-provider check
// ABOVE the rate branches. A provider that stopped (or crashed) while busy
// leaves a real billable_rate above threshold on disk, so the poll reports
// found=true; while the check sat inside `if !found` that stale rate reset the
// quiet timer on every poll and the tool looped until opts.Timeout — 30m in
// production — on a provider that is provably relaying nothing.
func TestWaitForIdleStoppedProviderWithStaleRate(t *testing.T) {
	oldPoll := idlePollInterval
	idlePollInterval = 5 * time.Millisecond
	t.Cleanup(func() { idlePollInterval = oldPoll })

	dir := t.TempDir()
	// The last sample this provider wrote before it stopped: 1 MiB/s, well
	// above the 5 KiB/s default threshold.
	if err := os.WriteFile(filepath.Join(dir, "billable_rate"), []byte("1048576\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rate, found, err := readBillableRateFile(dir); err != nil || !found || rate != 1048576 {
		t.Fatalf("stale rate not readable as expected: rate=%d found=%v err=%v", rate, found, err)
	}

	done := make(chan error, 1)
	go func() {
		done <- waitForIdle(context.Background(), dir, false, IdleUpdateOptions{
			Threshold: 5120,
			Window:    5 * time.Minute,
			Timeout:   time.Hour,
		}, nil)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stopped provider with a stale above-threshold rate: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForIdle did not return for a stopped provider whose stale billable_rate is above threshold")
	}
}

func TestWaitForIdleTimeout(t *testing.T) {
	ctx := context.Background()
	opts := IdleUpdateOptions{
		Threshold: 1000,
		Window:    10 * time.Minute,
		Timeout:   100 * time.Millisecond,
	}
	pollFn := func() (uint64, bool, error) {
		return 50000, true, nil // Always above threshold
	}
	start := time.Now()
	err := waitForIdle(ctx, "/tmp", true, opts, pollFn)
	if err != nil {
		t.Fatalf("expected graceful timeout return, got %v", err)
	}
	if time.Since(start) < 90*time.Millisecond {
		t.Fatalf("returned prematurely before timeout")
	}
}

// TestWaitForIdleMissingMetricReportsMissingDuration pins what the
// "waiting for initial rate metric" line means. It reports how long the metric
// has been MISSING, not how long this wait has run: a rate file that disappears
// after the tool has already been waiting a while used to print the total
// elapsed number (rounded to whole seconds), which next to a comment about a
// provider's 15s first tick reads as a timeout, not as two missed polls.
func TestWaitForIdleMissingMetricReportsMissingDuration(t *testing.T) {
	oldPoll := idlePollInterval
	idlePollInterval = 150 * time.Millisecond
	t.Cleanup(func() { idlePollInterval = oldPoll })

	// Found and quiet for a while, then missing for two polls, then found
	// again — the metric vanished mid-wait. The window is never satisfied, so
	// the wait ends on its timeout.
	polls := 0
	pollFn := func() (uint64, bool, error) {
		polls++
		switch polls {
		case 8, 9:
			return 0, false, nil
		default:
			return 0, true, nil
		}
	}

	out := captureStdout(t, func() {
		waitForIdle(context.Background(), "/tmp", true, IdleUpdateOptions{
			Threshold: 5120,
			Window:    5 * time.Minute,
			Timeout:   1500 * time.Millisecond,
		}, pollFn)
	})

	for _, want := range []string{"missing for 150ms", "missing for 300ms"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing-metric line does not report the missing duration %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "missing for 1s") || strings.Contains(out, "missing for 2s") {
		t.Fatalf("missing-metric line reported total elapsed instead of the missing duration:\n%s", out)
	}
}

func TestIdleUpdateDryRun(t *testing.T) {
	err := Run([]string{"idle-update", "--dry-run", "--unit=urnetwork.service"})
	if err != nil && err.Error() != "no providers found on this box" && !strings.Contains(err.Error(), "matches no provider") {
		t.Fatalf("unexpected idle-update dry-run error: %v", err)
	}
}

func TestDockerIdleUpdateDryRun(t *testing.T) {
	err := RunDocker([]string{"idle-update", "--dry-run", "--unit=test-provider"})
	if err != nil && err.Error() != "no provider containers found" && !strings.Contains(err.Error(), "matches no provider") {
		t.Fatalf("unexpected urnet-docker idle-update dry-run error: %v", err)
	}
}
