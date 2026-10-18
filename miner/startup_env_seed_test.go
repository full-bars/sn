package miner

import (
	"os"
	"path/filepath"
	"testing"
)

// The startup env seed is package-private and ran once at init(); these tests
// call it directly with a state directory pinned at a temp dir, the same way
// every other control-state test isolates itself.

func seedTestStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", dir)
	return dir
}

// clearProfileRamlogsEnv resets the two env vars seedEnvFromControlState
// writes, and restores them after the test via t.Setenv's cleanup - otherwise
// a value written by one test's seed call would leak into whichever test
// happens to run next in the same binary.
func clearProfileRamlogsEnv(t *testing.T) {
	t.Helper()
	t.Setenv("URNETWORK_PROFILE", "")
	t.Setenv("URNETWORK_RAMLOGS", "")
}

func writeSeedJSONFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestSeedEnvFromControlState_NoFilesIsNoop(t *testing.T) {
	seedTestStateDir(t)
	clearProfileRamlogsEnv(t)

	seedEnvFromControlState()

	if v := os.Getenv("URNETWORK_PROFILE"); v != "" {
		t.Errorf("URNETWORK_PROFILE = %q, want unset", v)
	}
	if v := os.Getenv("URNETWORK_RAMLOGS"); v != "" {
		t.Errorf("URNETWORK_RAMLOGS = %q, want unset", v)
	}
}

func TestSeedEnvFromControlState_ReadsPersistedProfile(t *testing.T) {
	dir := seedTestStateDir(t)
	clearProfileRamlogsEnv(t)

	writeSeedJSONFile(t, filepath.Join(dir, "provider_state.json"),
		`{"profile":"lowmem","ramlogs":"on"}`)

	seedEnvFromControlState()

	if v := os.Getenv("URNETWORK_PROFILE"); v != "lowmem" {
		t.Errorf("URNETWORK_PROFILE = %q, want %q", v, "lowmem")
	}
	if v := os.Getenv("URNETWORK_RAMLOGS"); v != "1" {
		t.Errorf("URNETWORK_RAMLOGS = %q, want %q", v, "1")
	}
}

// The v2 envelope is the format the control socket actually persists; the
// seed must read Values out of it, not mistake it for a legacy flat map.
func TestSeedEnvFromControlState_ReadsV2Envelope(t *testing.T) {
	dir := seedTestStateDir(t)
	clearProfileRamlogsEnv(t)

	writeSeedJSONFile(t, filepath.Join(dir, "provider_state.json"),
		`{"version":2,"values":{"profile":"turbo-v8","ramlogs":"off"},"meta":{"profile":{"source":"socket"}}}`)

	seedEnvFromControlState()

	if v := os.Getenv("URNETWORK_PROFILE"); v != "turbo-v8" {
		t.Errorf("URNETWORK_PROFILE = %q, want %q", v, "turbo-v8")
	}
	if v := os.Getenv("URNETWORK_RAMLOGS"); v != "0" {
		t.Errorf("URNETWORK_RAMLOGS = %q, want %q", v, "0")
	}
}

func TestSeedEnvFromControlState_RamlogsOffSetsZero(t *testing.T) {
	dir := seedTestStateDir(t)
	clearProfileRamlogsEnv(t)

	writeSeedJSONFile(t, filepath.Join(dir, "provider_state.json"),
		`{"ramlogs":"off"}`)

	seedEnvFromControlState()

	if v := os.Getenv("URNETWORK_RAMLOGS"); v != "0" {
		t.Errorf("URNETWORK_RAMLOGS = %q, want %q", v, "0")
	}
}

// TestSeedEnvFromControlState_PendingQueueTakesEffectOnFirstStart is the
// exact scenario the seed exists for: a fresh install where `urnet-tools set
// profile ...` was queued in pending_overrides.json because the provider was
// not running yet (no provider_state.json exists at all). The very first
// startup must still see it.
func TestSeedEnvFromControlState_PendingQueueTakesEffectOnFirstStart(t *testing.T) {
	dir := seedTestStateDir(t)
	clearProfileRamlogsEnv(t)

	writeSeedJSONFile(t, filepath.Join(dir, "pending_overrides.json"),
		`[{"op":"set","key":"profile","value":"turbo-v4"},{"op":"set","key":"ramlogs","value":"on"}]`)

	seedEnvFromControlState()

	if v := os.Getenv("URNETWORK_PROFILE"); v != "turbo-v4" {
		t.Errorf("URNETWORK_PROFILE = %q, want %q", v, "turbo-v4")
	}
	if v := os.Getenv("URNETWORK_RAMLOGS"); v != "1" {
		t.Errorf("URNETWORK_RAMLOGS = %q, want %q", v, "1")
	}

	// A read-only peek must not touch the queue file itself - that stays
	// mergePendingControlOverrides()'s job, inside the provide run.
	if _, err := os.Stat(filepath.Join(dir, "pending_overrides.json")); err != nil {
		t.Errorf("pending_overrides.json should be untouched by the peek, stat error: %v", err)
	}
}

// TestSeedEnvFromControlState_PendingQueueOverridesPersisted mirrors
// mergePendingControlOverrides()'s own precedence: a queued change on top of
// an already-persisted value wins, same order the real merge would apply.
func TestSeedEnvFromControlState_PendingQueueOverridesPersisted(t *testing.T) {
	dir := seedTestStateDir(t)
	clearProfileRamlogsEnv(t)

	writeSeedJSONFile(t, filepath.Join(dir, "provider_state.json"),
		`{"profile":"eco"}`)
	writeSeedJSONFile(t, filepath.Join(dir, "pending_overrides.json"),
		`[{"op":"set","key":"profile","value":"auto"},{"op":"clear","key":"ramlogs"}]`)

	seedEnvFromControlState()

	if v := os.Getenv("URNETWORK_PROFILE"); v != "auto" {
		t.Errorf("URNETWORK_PROFILE = %q, want %q (pending queue should win over persisted state)", v, "auto")
	}
}

func TestSeedEnvFromControlState_MalformedFilesAreIgnored(t *testing.T) {
	dir := seedTestStateDir(t)
	clearProfileRamlogsEnv(t)

	writeSeedJSONFile(t, filepath.Join(dir, "provider_state.json"), `not json`)
	writeSeedJSONFile(t, filepath.Join(dir, "pending_overrides.json"), `also not json`)

	// Must not panic, and must leave the env vars alone.
	seedEnvFromControlState()

	if v := os.Getenv("URNETWORK_PROFILE"); v != "" {
		t.Errorf("URNETWORK_PROFILE = %q, want unset on malformed input", v)
	}
}

// TestSeedEnvFromControlState_NullPersistedStateThenPendingSetDoesNotPanic
// covers a decode edge case distinct from "malformed JSON": a literal JSON
// `null` unmarshals successfully into a nil map (no error), so a naive
// unconditional `values = onDisk` assignment replaces the initialized
// non-nil `values` with nil. The pending-queue overlay right after this then
// panics on its first `values[op.Key] = op.Value` write into that nil map.
// Regression test for that exact sequence: null persisted state, followed by
// a queued set.
func TestSeedEnvFromControlState_NullPersistedStateThenPendingSetDoesNotPanic(t *testing.T) {
	dir := seedTestStateDir(t)
	clearProfileRamlogsEnv(t)

	writeSeedJSONFile(t, filepath.Join(dir, "provider_state.json"), `null`)
	writeSeedJSONFile(t, filepath.Join(dir, "pending_overrides.json"),
		`[{"op":"set","key":"profile","value":"turbo-v4"}]`)

	seedEnvFromControlState()

	if v := os.Getenv("URNETWORK_PROFILE"); v != "turbo-v4" {
		t.Errorf("URNETWORK_PROFILE = %q, want %q", v, "turbo-v4")
	}
}
