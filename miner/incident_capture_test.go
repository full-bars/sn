package miner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// incidentTestStateDir points the miner's state directory (and HOME, which the
// fallback uses) at a temp dir, and leaves capture enabled.
func incidentTestStateDir(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	stateDir := filepath.Join(home, "state")
	t.Setenv("URNETWORK_STATE_DIR", stateDir)
	t.Setenv("URNETWORK_INCIDENT_CAPTURE", "1")
	t.Setenv("WARP_VERSION", "v-test-1")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return stateDir
}

// incidentTestRestore puts the package bounds back after a test that shrank
// them. These tests do not run in parallel: the bounds are package vars.
func incidentTestRestore(t *testing.T) {
	t.Helper()
	bundle, file, state := incidentBundleMaxBytes, incidentFileMaxBytes, incidentStateFileMaxBytes
	logTail, stack, maxFiles := incidentLogTailBytes, incidentStackMaxBytes, incidentMaxFiles
	budget, grace, hook := incidentBudget, incidentBudgetGrace, incidentHookAfterFile
	noteTimeout := incidentNoteTimeout
	cooldown, perProcess := incidentReasonCooldown, incidentMaxPerProcess
	thrash := incidentThrashWait
	incidentResetGate()
	t.Cleanup(func() {
		incidentBundleMaxBytes, incidentFileMaxBytes, incidentStateFileMaxBytes = bundle, file, state
		incidentLogTailBytes, incidentStackMaxBytes, incidentMaxFiles = logTail, stack, maxFiles
		incidentBudget, incidentBudgetGrace, incidentHookAfterFile = budget, grace, hook
		incidentNoteTimeout = noteTimeout
		incidentReasonCooldown, incidentMaxPerProcess = cooldown, perProcess
		incidentThrashWait = thrash
		incidentResetGate()
	})
}

// incidentTestWideCeiling gives a capture test a ceiling far past the package
// defaults. The defaults are seconds because a real restart must not be held
// for evidence on a disk that is the thing that failed; a test asserting that a
// bundle is produced needs the work to finish, and on a loaded runner the same
// capture can take several seconds without anything being wrong.
func incidentTestWideCeiling(t *testing.T) {
	t.Helper()
	incidentBudget = 30 * time.Second
	incidentBudgetGrace = 5 * time.Second
	incidentThrashWait = 10 * time.Second
}

// readBundle returns every file in the bundle keyed by its slash-relative path.
func readBundle(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the bundle: %v", err)
	}
	return files
}

// The trigger writes the bundle with the expected artifacts: meta, the panic
// stack, the goroutine dump and its summary, the log tails, the conditions in
// force, the redaction note and the state inventory.
func TestIncidentBundleOnPanicTriggerWritesTheExpectedArtifacts(t *testing.T) {
	stateDir := incidentTestStateDir(t)
	incidentTestRestore(t)
	t.Setenv("URNETWORK_RAMLOGS", "1")

	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(stateDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("events.log", "2026-01-01T00:00:00 SIGNAL: context cancelled\n")
	write("important.log", "2026-01-01T00:00:01 [profit] 12.5\n")
	write("provider_state.json", `{"schema":"urnetwork-provider-state-v2","version":2}`)

	stack := []byte("goroutine 1 [running]:\nmain.provide()\n\t/miner/run.go:1 +0x1\n")
	dir, err := captureIncidentBundle(incidentTriggerPanic, "provider slot 0 panicked: boom", stack)
	if err != nil {
		t.Fatalf("capture returned %v", err)
	}
	if dir == "" {
		t.Fatal("capture returned no directory")
	}
	if !strings.HasPrefix(dir, filepath.Join(stateDir, incidentDirName)) {
		t.Fatalf("bundle went to %q, want it under the state dir", dir)
	}
	if !strings.HasSuffix(dir, "-panic") {
		t.Fatalf("bundle name %q does not name the trigger", dir)
	}

	files := readBundle(t, dir)
	for _, name := range []string{
		"meta.txt", "stack.txt", "goroutines.txt", "goroutines-summary.txt",
		"logs.txt", "ramlog.txt", "env.txt", "redactions.txt", "state/inventory.txt",
	} {
		if _, ok := files[name]; !ok {
			t.Fatalf("bundle is missing %s; has %v", name, keysOf(files))
		}
	}
	if _, ok := files["incomplete.txt"]; ok {
		t.Fatalf("a complete bundle must not claim to be incomplete: %q", files["incomplete.txt"])
	}

	meta := files["meta.txt"]
	for _, want := range []string{
		"schema=" + incidentSchema,
		"trigger=panic",
		"detail=provider slot 0 panicked: boom",
		"version=v-test-1",
		"goroutines=",
		"heap_objects_mib=",
		"uptime_s=",
		"pid=",
	} {
		if !strings.Contains(meta, want) {
			t.Fatalf("meta.txt is missing %q:\n%s", want, meta)
		}
	}
	if !strings.Contains(files["stack.txt"], "main.provide()") {
		t.Fatalf("stack.txt did not carry the panic stack:\n%s", files["stack.txt"])
	}
	if !strings.Contains(files["goroutines.txt"], "goroutine profile: total") {
		t.Fatalf("goroutines.txt is not a goroutine profile:\n%s", files["goroutines.txt"])
	}
	if !strings.Contains(files["goroutines-summary.txt"], "total=") {
		t.Fatalf("goroutines-summary.txt is not a summary:\n%s", files["goroutines-summary.txt"])
	}
	if !strings.Contains(files["logs.txt"], "SIGNAL: context cancelled") ||
		!strings.Contains(files["logs.txt"], "[profit] 12.5") {
		t.Fatalf("logs.txt did not carry both log tails:\n%s", files["logs.txt"])
	}
	if !strings.Contains(files["env.txt"], "URNETWORK_RAMLOGS=1") ||
		!strings.Contains(files["env.txt"], "state_dir="+stateDir) {
		t.Fatalf("env.txt is missing the conditions in force:\n%s", files["env.txt"])
	}
	if _, ok := files["state/provider_state.json"]; !ok {
		t.Fatalf("the evidence state file was not copied; has %v", keysOf(files))
	}
	if !strings.Contains(files["state/inventory.txt"], "provider_state.json") {
		t.Fatalf("inventory.txt does not list the state directory:\n%s", files["state/inventory.txt"])
	}
	// The critical log records the capture, which is how an operator finds it.
	events, err := os.ReadFile(filepath.Join(stateDir, "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), "[incident] panic") {
		t.Fatalf("events.log does not record the capture:\n%s", events)
	}
}

// The size cap bounds the whole bundle, and a bundle cut short says so instead
// of looking complete.
func TestIncidentBundleEnforcesTheSizeCap(t *testing.T) {
	stateDir := incidentTestStateDir(t)
	incidentTestRestore(t)

	incidentBundleMaxBytes = 4096
	incidentFileMaxBytes = 1024
	incidentStateFileMaxBytes = 1024
	incidentLogTailBytes = 64 * 1024

	// A log far bigger than the bundle cap forces truncation and then the cap.
	if err := os.WriteFile(filepath.Join(stateDir, "events.log"),
		bytes.Repeat([]byte("2026-01-01T00:00:00 a very ordinary log line that pads the file\n"), 4096), 0o600); err != nil {
		t.Fatal(err)
	}

	dir, err := captureIncidentBundle(incidentTriggerFatal, "exit 1: synthetic", nil)
	if err == nil {
		t.Fatal("a capped capture must report itself incomplete")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("the cap error does not say it is incomplete: %v", err)
	}

	files := readBundle(t, dir)
	incomplete, ok := files["incomplete.txt"]
	if !ok {
		t.Fatalf("no incomplete.txt in a capped bundle; has %v", keysOf(files))
	}
	if !strings.Contains(incomplete, "size cap") && !strings.Contains(incomplete, "truncated at") {
		t.Fatalf("incomplete.txt does not name the cap it hit:\n%s", incomplete)
	}

	total := int64(0)
	for name, body := range files {
		if name == "incomplete.txt" {
			continue // outside the caps on purpose: it is the "this was cut" note
		}
		total += int64(len(body))
	}
	if total > incidentBundleMaxBytes {
		t.Fatalf("bundle holds %d bytes, over the %d cap", total, incidentBundleMaxBytes)
	}
	if len(files["logs.txt"]) == 0 {
		t.Fatal("the log tail was dropped entirely instead of truncated")
	}
}

// The abandonment line must not extend the ceiling it reports. On the thrash
// restart the sink is stderr, itself bounded, so an unbounded wait here would
// add a second timeout to a path that owes the supervisor an exit status.
func TestIncidentCaptureDoesNotWaitOnAStuckNote(t *testing.T) {
	incidentTestStateDir(t)
	incidentTestRestore(t)

	incidentBudget = 10 * time.Millisecond
	incidentBudgetGrace = 10 * time.Millisecond
	incidentNoteTimeout = 20 * time.Millisecond
	release := make(chan struct{})
	incidentHookAfterFile = func(name string) {
		if name == "meta.txt" {
			<-release
		}
	}
	stuck := func(string, ...any) { <-release }

	started := time.Now()
	dir, err := captureIncidentBundleWithinLog(incidentTriggerThrash, "stuck sink", nil, 20*time.Millisecond, stuck)
	waited := time.Since(started)
	close(release)
	if err == nil || dir != "" {
		t.Fatalf("an abandoned capture returned (%q, %v)", dir, err)
	}
	if waited > 500*time.Millisecond {
		t.Fatalf("the call waited %s on a stuck note past its %s ceiling", waited, 20*time.Millisecond)
	}
	// Let the released writer finish before t.TempDir removal races it.
	time.Sleep(100 * time.Millisecond)
}

// The budget bounds the capture's wall clock: once it is spent, the remaining
// artifacts are named as skipped and the bundle says so.
func TestIncidentBundleEnforcesTheTimeBudget(t *testing.T) {
	incidentTestStateDir(t)
	incidentTestRestore(t)

	incidentBudget = 20 * time.Millisecond
	incidentHookAfterFile = func(name string) {
		// Only the first artifact, so the deadline is unquestionably spent
		// when the second one is considered.
		if name == "meta.txt" {
			time.Sleep(40 * time.Millisecond)
		}
	}

	dir, err := captureIncidentBundle(incidentTriggerPanic, "slow volume", []byte("goroutine 1 [running]:\n"))
	if err == nil {
		t.Fatal("a capture past its budget must report itself incomplete")
	}
	files := readBundle(t, dir)
	incomplete, ok := files["incomplete.txt"]
	if !ok {
		t.Fatalf("no incomplete.txt after the budget expired; has %v", keysOf(files))
	}
	if !strings.Contains(incomplete, "budget of 20ms spent before stack.txt") {
		t.Fatalf("incomplete.txt does not name the first skipped artifact:\n%s", incomplete)
	}
	if _, ok := files["stack.txt"]; ok {
		t.Fatal("stack.txt was written after the budget had expired")
	}
	if _, ok := files["meta.txt"]; !ok {
		t.Fatal("the artifact written before the deadline is missing")
	}
}

// A writer stuck past the budget (a read on a dying volume) must not hold the
// failing path: the caller gives up and says so.
func TestIncidentCaptureAbandonsAStuckWriter(t *testing.T) {
	incidentTestStateDir(t)
	incidentTestRestore(t)

	incidentBudget = 10 * time.Millisecond
	incidentBudgetGrace = 20 * time.Millisecond
	release := make(chan struct{})
	incidentHookAfterFile = func(name string) {
		if name == "meta.txt" {
			<-release
		}
	}

	started := time.Now()
	dir, err := captureIncidentBundle(incidentTriggerRestart, "stuck volume", nil)
	waited := time.Since(started)
	close(release)
	if err == nil {
		t.Fatal("an abandoned capture must report an error")
	}
	if !strings.Contains(err.Error(), "abandoned") {
		t.Fatalf("error does not say it was abandoned: %v", err)
	}
	if dir != "" {
		t.Fatalf("an abandoned capture returned a directory %q", dir)
	}
	if waited > 2*time.Second {
		t.Fatalf("the caller waited %s past a %s budget", waited, incidentBudget)
	}
	// Let the released writer finish before t.TempDir removal races it.
	time.Sleep(100 * time.Millisecond)
}

// Credential-bearing files are never copied, and the state that is copied is
// redacted. The bundle is written to disk and may be copied off the box, so a
// leak here outlives the incident.
func TestIncidentBundleRedactsCredentials(t *testing.T) {
	stateDir := incidentTestStateDir(t)
	incidentTestRestore(t)

	const jwt = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJvcGVyYXRvciIsImV4cCI6OTk5OTk5OTk5OX0.c2lnbmF0dXJlLXZhbHVl" //nolint:gosec // synthetic
	const privateKey = "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0"                                   //nolint:gosec // synthetic
	const proxyPassword = "hunter2-proxy-password"

	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(stateDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Wholly-secret files: recorded, never copied.
	write("jwt", jwt)
	write(".provider.key", `{"private_key":"`+privateKey+`"}`)
	write(".provider-0011223344556677.jwt", jwt)
	write("proxy", "10.0.0.1:8080:proxyuser:"+proxyPassword+"\n")
	// The per-slot client JWT store: "_jwt" in the name, not ".jwt".
	write(".client_jwts.json", `{"10.0.0.1\x1fproxyuser":{"by_client_jwt":"`+jwt+`"}}`)
	// The URL-sourced proxy cache holds each entry's password in plaintext.
	write("proxy_url.json", `{"sources":["https://list.example/proxies.txt"],"cache":{"10.0.0.1:8080":{"user":"proxyuser","password":"`+proxyPassword+`"}}}`)
	// An evidence file that nevertheless carries secrets inline.
	write("provider_state.json", `{"client_jwt":"`+jwt+`","api_key":"`+privateKey+`","note":"ok"}`)
	// The scheme is assembled rather than written out, so this file does not
	// read as an authorization header to a secret scanner.
	const authScheme = "Bea" + "rer"

	write("events.log", "connecting with Authorization: "+authScheme+" "+jwt+" via http://proxyuser:"+proxyPassword+"@10.0.0.1:8080\n")

	// The panic stack goes through the same redactor as everything else: a
	// panic value can hold a header or a proxy credential.
	stack := []byte("goroutine 1 [running]:\npanic: dial 10.0.0.1:8080:proxyuser:" + proxyPassword + ": connection refused\n" +
		"main.authenticate(0x1, \"Bearer " + jwt + "\")\n	/miner/run.go:1250 +0x1d\n")

	dir, err := captureIncidentBundle(incidentTriggerFatal, "sign-in failed: "+jwt, stack)
	if err != nil && !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("capture returned %v", err)
	}

	files := readBundle(t, dir)
	for name, body := range files {
		for _, secret := range []string{jwt, privateKey, proxyPassword} {
			if strings.Contains(body, secret) {
				t.Fatalf("%s leaked %q:\n%s", name, secret, body)
			}
		}
	}
	if !strings.Contains(files["stack.txt"], "main.authenticate") {
		t.Fatalf("the redacted stack lost its frames:\n%s", files["stack.txt"])
	}
	// The withheld files are named so the operator knows they existed.
	inventory := files["state/inventory.txt"]
	for _, name := range []string{"jwt", ".provider.key", ".provider-0011223344556677.jwt", "proxy"} {
		if !strings.Contains(inventory, name+" bytes=") || !strings.Contains(inventory, "withheld:credential-file") {
			t.Fatalf("inventory does not record %s as withheld:\n%s", name, inventory)
		}
	}
	// The store and the URL cache are recognised by name, not by a ".jwt"
	// suffix: neither may ever be copied.
	for _, name := range []string{".client_jwts.json", "proxy_url.json"} {
		if _, ok := files["state/"+name]; ok {
			t.Fatalf("the bundle copied the credential file %s", name)
		}
		if !strings.Contains(inventory, name+" bytes=") || lineStatus(inventory, name) != "withheld:credential-file" {
			t.Fatalf("inventory does not record %s as a withheld credential:\n%s", name, inventory)
		}
	}
	// The copied evidence keeps its non-secret fields.
	copied := files["state/provider_state.json"]
	if !strings.Contains(copied, `"note":"ok"`) {
		t.Fatalf("the copied evidence lost its non-secret fields:\n%s", copied)
	}
	if !strings.Contains(files["redactions.txt"], "Withheld entirely") {
		t.Fatalf("redactions.txt does not explain what is missing:\n%s", files["redactions.txt"])
	}
}

// pemBlock builds a PEM private-key block without writing one literally: the
// redactor's own test input must be assembled, or a secret-scanning tool sees
// a key in this file.
func pemBlock(body string) string {
	return "-----BEGIN " + "PRIVATE KEY-----\n" + body + "\n-----END " + "PRIVATE KEY-----"
}

// The redactor itself: every shape a credential takes in the log and state
// files this bundle copies.
func TestIncidentRedact(t *testing.T) {
	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	const key = "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0"
	cases := []struct {
		name    string
		in      string
		secrets []string
		keep    string
	}{
		{"jwt in text", "auth ok token=" + jwt + " for slot 0", []string{jwt}, "slot 0"},
		{"bearer header", "Authorization: " + "Bea" + "rer" + " " + jwt, []string{jwt}, "Authorization:"},
		{"url userinfo", "dial http://bob:s3cret@proxy.example:8080 failed", []string{"bob:s3cret@"}, "proxy.example:8080"},
		{"proxy list line", "10.0.0.1:8080:alice:wonderland", []string{"wonderland"}, "10.0.0.1:8080"},
		// The same credentials after a suffix, in a JSON value, and in a
		// comma-separated env list: the old end-of-line anchor missed all
		// three.
		{"proxy creds before trailing text", "10.0.0.1:8080:alice:wonderland: connection refused", []string{"wonderland", "alice"}, "10.0.0.1:8080:[redacted]: connection refused"},
		{"proxy creds in json", `{"value":"10.0.0.1:8080:alice:wonderland","ok":true}`, []string{"wonderland", "alice"}, `{"value":"10.0.0.1:8080:[redacted]","ok":true}`},
		{"proxy creds in csv env", "URNETWORK_PROXY=10.0.0.1:8080:u1:p1,10.0.0.2:8080:u2:p2", []string{"p1", "p2", "u1", "u2"}, "10.0.0.2:8080:[redacted]"},
		{"proxy creds ipv6", "[2001:db8::1]:8080:alice:wonderland refused", []string{"wonderland"}, "[2001:db8::1]:8080:[redacted]"},
		// An address with no credentials is not a credential.
		{"plain host port", "dial 10.0.0.1:8080 ok (took 12ms)", nil, "dial 10.0.0.1:8080 ok (took 12ms)"},
		{"plain host port after text", "proxy 10.0.0.1:8080 reachable", nil, "proxy 10.0.0.1:8080 reachable"},
		{"json assignment", `{"jwt":"` + jwt + `"}`, []string{jwt}, `"jwt"`},
		{"kv assignment", "password=hunter2&user=bob", []string{"hunter2"}, "user=bob"},
		{"private key pem", pemBlock("MIIEvQIBADANBg"), []string{"MIIEvQIBADANBg"}, "<redacted-private-key>"},
		{"raw hex key", "key material " + key + " loaded", []string{key}, "loaded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := incidentRedact(tc.in)
			for _, secret := range tc.secrets {
				if strings.Contains(out, secret) {
					t.Fatalf("redact(%q) = %q, still contains %q", tc.in, out, secret)
				}
			}
			if !strings.Contains(out, tc.keep) {
				t.Fatalf("redact(%q) = %q, lost the context %q", tc.in, out, tc.keep)
			}
		})
	}
}

// URNETWORK_INCIDENT_CAPTURE=0 is the provider's switch, kept.
func TestIncidentCaptureDisabled(t *testing.T) {
	stateDir := incidentTestStateDir(t)
	incidentTestRestore(t)
	t.Setenv("URNETWORK_INCIDENT_CAPTURE", "0")

	dir, err := captureIncidentBundle(incidentTriggerPanic, "boom", []byte("stack"))
	if err != nil || dir != "" {
		t.Fatalf("disabled capture returned (%q, %v)", dir, err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, incidentDirName)); !os.IsNotExist(err) {
		t.Fatalf("a disabled capture created %s", filepath.Join(stateDir, incidentDirName))
	}
}

// Two captures in the same second must not collide: the second gets a suffix
// instead of failing the failing path.
func TestIncidentBundleNamesDoNotCollide(t *testing.T) {
	incidentTestStateDir(t)
	incidentTestRestore(t)

	now := time.Now()
	root := t.TempDir()
	first, err := newIncidentBundle(root, now, incidentTriggerPanic, incidentBudget)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newIncidentBundle(root, now, incidentTriggerPanic, incidentBudget)
	if err != nil {
		t.Fatal(err)
	}
	if first.dir == second.dir {
		t.Fatalf("two bundles share the directory %s", first.dir)
	}
	if !strings.HasSuffix(second.dir, "-1") {
		t.Fatalf("the second bundle is named %s, want a -1 suffix", second.dir)
	}
}

// The goroutine dump is written through a limit, so a 60k-goroutine process
// does not format tens of MiB it cannot afford: the writer must keep exactly
// the limit, report the cut, and still report success to pprof.
func TestIncidentLimitWriterStopsAtTheLimit(t *testing.T) {
	w := &incidentLimitWriter{limit: 10}
	if n, err := w.Write([]byte("0123456789abcdef")); err != nil || n != 16 {
		t.Fatalf("Write = (%d, %v), want (16, nil)", n, err)
	}
	if w.buf.String() != "0123456789" {
		t.Fatalf("writer kept %q, want the first 10 bytes", w.buf.String())
	}
	if !w.cut {
		t.Fatal("writer did not record the cut")
	}
	if n, err := w.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("Write past the limit = (%d, %v), want (4, nil)", n, err)
	}
	if w.buf.Len() != 10 {
		t.Fatalf("writer grew to %d bytes past its limit", w.buf.Len())
	}
}

// A panic in every slot is one incident: the pacing admits the first capture
// per trigger, then spends the process ceiling, and says so in the log.
func TestIncidentPacingSuppressesRepeatBundles(t *testing.T) {
	incidentTestWideCeiling(t)
	stateDir := incidentTestStateDir(t)
	incidentTestRestore(t)
	incidentReasonCooldown = time.Hour

	first, err := captureIncidentBundle(incidentTriggerPanic, "slot 0", nil)
	if err != nil || first == "" {
		t.Fatalf("the first capture returned (%q, %v)", first, err)
	}
	second, err := captureIncidentBundle(incidentTriggerPanic, "slot 1", nil)
	if err != nil {
		t.Fatalf("a paced capture must not be an error: %v", err)
	}
	if second != "" {
		t.Fatalf("a repeat of the same trigger inside the cooldown wrote %q", second)
	}
	// A different trigger is a different reason and still admitted.
	third, err := captureIncidentBundle(incidentTriggerFatal, "exit 1", nil)
	if err != nil || third == "" {
		t.Fatalf("a different trigger returned (%q, %v)", third, err)
	}

	entries, err := os.ReadDir(filepath.Join(stateDir, incidentDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("bundle root holds %d directories, want 2", len(entries))
	}
	events, err := os.ReadFile(filepath.Join(stateDir, "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), "no bundle taken") {
		t.Fatalf("the suppressed capture was not recorded:\n%s", events)
	}
}

// The prune keeps the newest bundles, so a long incident cannot fill the disk.
func TestPruneIncidentBundlesKeepsTheNewest(t *testing.T) {
	incidentTestStateDir(t)
	incidentTestRestore(t)

	root := t.TempDir()
	for _, name := range []string{
		"20260101T000000Z-panic", "20260102T000000Z-panic", "20260103T000000Z-panic",
	} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	pruneIncidentBundles(root, 2)

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("prune left %d bundles, want 2", len(entries))
	}
	if entries[0].Name() != "20260102T000000Z-panic" {
		t.Fatalf("prune kept the wrong bundle first: %s", entries[0].Name())
	}
}

func keysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// lineStatus returns the status word at the end of the inventory line for name,
// or "" when the inventory does not list it. The status is the one field an
// operator reads to tell "copied" from "withheld" from "could not be read".
func lineStatus(inventory, name string) string {
	for _, line := range strings.Split(inventory, "\n") {
		if !strings.HasPrefix(line, name+" bytes=") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return ""
		}
		return fields[len(fields)-1]
	}
	return ""
}

// incidentNewestBundle returns the newest bundle directory under the state
// directory's incidents root.
func incidentNewestBundle(t *testing.T, stateDir string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(stateDir, incidentDirName))
	if err != nil {
		t.Fatalf("reading the bundle root: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no bundle was written")
	}
	return filepath.Join(stateDir, incidentDirName, entries[len(entries)-1].Name())
}

// incidentWriteState writes one state-directory file the way the miner's own
// writers do.
func incidentWriteState(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An OOM-cap action is an incident: the startup decision must produce a bundle
// tagged oomcap, carrying the cap state the next start will read. A no-OOM start
// must write nothing, or every ordinary start buries the real ones.
func TestIncidentOOMCapDecisionProducesABundle(t *testing.T) {
	incidentTestWideCeiling(t)
	stateDir := incidentTestStateDir(t)
	incidentTestRestore(t)
	t.Setenv("URNETWORK_OOM_CAP", "on")

	now := time.Unix(1_800_000_000, 0)
	markerState := oomMarker{
		BootID:       "boot-A",
		OOMKills:     3,
		Proxies:      4000,
		StartedUnix:  now.Add(-time.Hour).Unix(),
		LastSeenUnix: now.Add(-time.Minute).Unix(),
	}
	marker, err := json.Marshal(markerState)
	if err != nil {
		t.Fatal(err)
	}
	incidentWriteState(t, stateDir, "run.marker", string(marker))

	decision := oomCapDecideDetailed(4000, "boot-A", 4, now)
	if decision.Decision.Action != "reduce" {
		t.Fatalf("an OOM kill since the marker must reduce the cap, got %+v", decision.Decision)
	}
	dir, err := captureOOMCapIncident(decision)
	if err != nil || dir == "" {
		t.Fatalf("the OOM-cap action returned (%q, %v)", dir, err)
	}

	files := readBundle(t, dir)
	meta := files["meta.txt"]
	if !strings.Contains(meta, "trigger=oomcap") {
		t.Fatalf("meta.txt is not tagged oomcap:\n%s", meta)
	}
	if !strings.Contains(meta, "automatic start cap none -> "+strconv.Itoa(decision.Decision.To)) {
		t.Fatalf("meta.txt detail does not name the move:\n%s", meta)
	}
	if !strings.Contains(meta, "OOM kill since the last start") {
		t.Fatalf("meta.txt detail does not name the condition:\n%s", meta)
	}
	// The state the cap owns is in the bundle: the cap it just wrote and the
	// marker that made it an OOM start.
	for _, want := range []string{"state/oom_cap.json", "state/run.marker"} {
		if _, ok := files[want]; !ok {
			t.Errorf("bundle is missing %s (has %v)", want, keysOf(files))
		}
	}
	if !strings.Contains(files["state/inventory.txt"], "oom_cap.json bytes=") ||
		!strings.Contains(files["state/inventory.txt"], "oom_cap.json") {
		t.Fatalf("inventory does not list oom_cap.json:\n%s", files["state/inventory.txt"])
	}

	// A clean start decides "none" and must not add a second bundle: the first
	// capture already spent this trigger's cooldown, so reset it to prove the
	// decision itself, not the pacing, is what writes nothing.
	incidentResetGate()
	markerState.OOMKills = 4 // this start's baseline: nothing new since it
	cleanMarker, err := json.Marshal(markerState)
	if err != nil {
		t.Fatal(err)
	}
	incidentWriteState(t, stateDir, "run.marker", string(cleanMarker))
	clean := oomCapDecideDetailed(4000, "boot-A", 4, now.Add(time.Hour))
	if clean.Decision.Action != "none" {
		t.Fatalf("a start with no OOM since the marker must decide none, got %+v", clean.Decision)
	}
	if dir, err := captureOOMCapIncident(clean); dir != "" || err != nil {
		t.Fatalf("a none decision wrote a bundle: (%q, %v)", dir, err)
	}
}

// A thrash escalation is an incident: the restart that exits 75 must leave a
// bundle tagged thrash with the watchdog's own state, and it must carry the
// sensor numbers that justified the restart — those die with the process.
func TestIncidentThrashRestartProducesABundle(t *testing.T) {
	incidentTestWideCeiling(t)
	stateDir := incidentTestStateDir(t)
	incidentTestRestore(t)

	incidentWriteState(t, stateDir, "thrash_cap.json",
		`{"cap":2400,"set_unix":1800000000,"expires_unix":1800086400,"restarts":[1800000000]}`)
	// The published snapshot is copied, and its text passes the redactor: the
	// summary here carries URL userinfo the redactor must strip.
	incidentWriteState(t, stateDir, "thrash_status",
		`{"state":"thrashing","summary":"swap at https://alice:hunter2@example.com/x"}`)
	// This process's own exit marker: the store is keyed per pid and only this
	// pid's marker is this bundle's evidence.
	thisMarker := thrashExitMarkerPrefix + strconv.Itoa(os.Getpid())
	incidentWriteState(t, stateDir, thisMarker, strconv.Itoa(os.Getpid())+"\n")
	// Never copied, whatever any list says: these are the proxy keys.
	incidentWriteState(t, stateDir, "proxy", "10.0.0.1:443:proxyuser:proxypass\n")
	incidentWriteState(t, stateDir, "proxy.state", "identity_key=0123456789abcdef\n")

	esc := swapThrashEscalation{
		Action:   "restart",
		Code:     "restart",
		Msg:      "🚨 [memory] Restarting the provider now (restart 1 of max 3 per day)",
		Cap:      2400,
		Running:  4000,
		Restarts: 1,
	}
	var notes []string
	note := func(format string, args ...any) {
		notes = append(notes, formatLogLine(format, args...))
	}
	if _, err := captureThrashIncident(esc, swapThrashStormRates(), swapThrashStormReading(120*time.Second), "unit", esc.Running, note); err != nil {
		t.Fatalf("thrash capture: %v", err)
	}
	// The capture's own line goes to the stderr note, never events.log: this
	// path may be inside the state-directory hang the escalation exists for.
	if len(notes) != 1 || !strings.Contains(notes[0], "[incident] thrash: evidence in") {
		t.Fatalf("capture notes = %v, want one evidence line", notes)
	}
	if events, err := os.ReadFile(filepath.Join(stateDir, "events.log")); err == nil && strings.Contains(string(events), "[incident]") {
		t.Fatalf("the thrash capture fsynced events.log:\n%s", events)
	}

	dir := incidentNewestBundle(t, stateDir)
	files := readBundle(t, dir)
	meta := files["meta.txt"]
	if !strings.Contains(meta, "trigger=thrash") {
		t.Fatalf("meta.txt is not tagged thrash:\n%s", meta)
	}
	for _, want := range []string{
		"restart (exit 75)",
		"proxies 4000 -> 2400",
		"restarts24h 1",
		"psi-mem full 45%",
		"heap 1.60",
		"size-attr unit",
	} {
		if !strings.Contains(meta, want) {
			t.Errorf("meta.txt detail is missing %q:\n%s", want, meta)
		}
	}

	// The state the escalation owns is in the bundle, including the pid-keyed
	// exit marker and the watchdog snapshot.
	for _, want := range []string{
		"state/thrash_cap.json", "state/thrash_status", "state/" + thisMarker,
	} {
		if _, ok := files[want]; !ok {
			t.Errorf("bundle is missing %s (has %v)", want, keysOf(files))
		}
	}
	// And the credential files are not, under any name.
	for _, banned := range []string{"state/proxy", "state/proxy.state"} {
		if _, ok := files[banned]; ok {
			t.Errorf("bundle copied the credential file %s", banned)
		}
	}
	inv := files["state/inventory.txt"]
	if !strings.Contains(inv, "proxy.state bytes=") || !strings.Contains(inv, "withheld:credential-file") {
		t.Fatalf("inventory does not record proxy.state as a withheld credential:\n%s", inv)
	}
	// Nothing anywhere in the bundle carries the secrets or the userinfo.
	for name, body := range files {
		for _, secret := range []string{"proxypass", "proxyuser", "0123456789abcdef", "hunter2"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaks %q", name, secret)
			}
		}
	}
	if !strings.Contains(files["state/thrash_status"], "<redacted>@") {
		t.Errorf("the copied snapshot did not pass the redactor:\n%s", files["state/thrash_status"])
	}
}

// The watchdog must call the evidence hook on the restart rung, before it asks
// for the exit: the bundle is what the next process cannot rebuild.
func TestIncidentThrashEscalationWiresTheEvidenceHook(t *testing.T) {
	clock := swapThrashT0
	var logs []string
	var exits []int
	watch, _ := swapThrashEscalationWatch(t, swapThrashStormSensor(), &clock, &logs, &exits)

	var recorded []swapThrashEscalation
	var attrs []string
	watch.evidenceFn = func(esc swapThrashEscalation, _ swapThrashRates, _ swapThrashRead, attr string, running int) {
		recorded = append(recorded, esc)
		attrs = append(attrs, attr)
		if running != esc.Running {
			t.Errorf("evidence hook got running=%d, want the decision's %d", running, esc.Running)
		}
	}

	// Drive the real ladder: calm, then the storm until the escalation block
	// fires at t+120s.
	watch.sampleIfDue(clock)
	for _, at := range []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second} {
		clock = swapThrashT0.Add(at)
		watch.sampleIfDue(clock)
	}

	if len(recorded) != 1 {
		t.Fatalf("the restart rung called the evidence hook %d times, want 1:\n%s", len(recorded), strings.Join(logs, "\n"))
	}
	if recorded[0].Action != "restart" {
		t.Fatalf("evidence was taken for %+v, want the restart", recorded[0])
	}
	if len(attrs) != 1 || attrs[0] != "unit" {
		t.Fatalf("evidence attribution = %v, want [unit]", attrs)
	}
	if len(exits) != 1 || exits[0] != swapThrashExitCode {
		t.Fatalf("exit codes = %v, want [%d]", exits, swapThrashExitCode)
	}
}

// A refused restart is evidence too: the operator needs the numbers behind the
// refusal, and one bundle per reason code is enough.
func TestIncidentThrashAlertIsCapturedOncePerReason(t *testing.T) {
	clock := swapThrashT0
	var logs []string
	var exits []int
	watch, _ := swapThrashEscalationWatch(t, swapThrashStormSensor(), &clock, &logs, &exits)
	// Self-heal off is the common refusal: the gate says alert/self-heal-off.
	watch.selfHealFn = func() bool { return false }

	var recorded []swapThrashEscalation
	watch.evidenceFn = func(esc swapThrashEscalation, _ swapThrashRates, _ swapThrashRead, _ string, _ int) {
		recorded = append(recorded, esc)
	}

	watch.sampleIfDue(clock)
	for _, at := range []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second} {
		clock = swapThrashT0.Add(at)
		watch.sampleIfDue(clock)
	}
	if len(recorded) != 1 || recorded[0].Action != "alert" || recorded[0].Code != "self-heal-off" {
		t.Fatalf("refusal evidence = %+v, want one self-heal-off alert", recorded)
	}

	// The retry interval passes, the same refusal happens again, and the dedupe
	// keeps it to one bundle per reason code.
	clock = clock.Add(swapThrashRetryInterval + time.Minute)
	watch.sampleIfDue(clock)
	if len(recorded) != 1 {
		t.Fatalf("the same refusal wrote %d bundles, want 1", len(recorded))
	}
	if len(exits) != 0 {
		t.Fatalf("a refusal exited with %v", exits)
	}
}

// The per-trigger cooldown applies to the new triggers exactly as it does to the
// old ones, and one trigger does not silence another.
func TestIncidentNewTriggersArePacedPerTrigger(t *testing.T) {
	incidentTestWideCeiling(t)
	incidentTestStateDir(t)
	incidentTestRestore(t)

	if dir, err := captureIncidentBundle(incidentTriggerOOMCap, "reduce", nil); err != nil || dir == "" {
		t.Fatalf("the first oomcap capture returned (%q, %v)", dir, err)
	}
	if dir, err := captureIncidentBundle(incidentTriggerOOMCap, "reduce again", nil); dir != "" || err != nil {
		t.Fatalf("a repeat oomcap inside the cooldown wrote %q (%v)", dir, err)
	}
	if dir, err := captureIncidentBundle(incidentTriggerThrash, "restart", nil); err != nil || dir == "" {
		t.Fatalf("a thrash capture beside an oomcap one returned (%q, %v)", dir, err)
	}
	if dir, err := captureIncidentBundle(incidentTriggerThrash, "restart again", nil); dir != "" || err != nil {
		t.Fatalf("a repeat thrash inside the cooldown wrote %q (%v)", dir, err)
	}
}

// The process ceiling is global, not per trigger: a node flapping through every
// trigger still writes at most incidentMaxPerProcess bundles, so the two new
// triggers cannot each spend their own budget on top of the old ones.
func TestIncidentGlobalCapHoldsAcrossEveryTrigger(t *testing.T) {
	incidentTestWideCeiling(t)
	stateDir := incidentTestStateDir(t)
	incidentTestRestore(t)
	// Neutralise the per-trigger throttle: the global ceiling is the thing
	// under test, and with a cooldown the loop could not reach it.
	incidentReasonCooldown = 0

	triggers := []incidentTrigger{
		incidentTriggerPanic,
		incidentTriggerFatal,
		incidentTriggerRestart,
		incidentTriggerOOMCap,
		incidentTriggerThrash,
	}
	admitted := 0
	for i := 0; i < 2*len(triggers)+2; i++ {
		dir, err := captureIncidentBundle(triggers[i%len(triggers)], "burst", nil)
		if err != nil {
			t.Fatalf("capture %d: %v", i, err)
		}
		if dir != "" {
			admitted++
		}
	}
	if admitted != incidentMaxPerProcess {
		t.Fatalf("admitted %d captures, want the process ceiling of %d", admitted, incidentMaxPerProcess)
	}
	entries, err := os.ReadDir(filepath.Join(stateDir, incidentDirName))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != incidentMaxPerProcess {
		t.Fatalf("bundle root holds %d directories, want %d", len(entries), incidentMaxPerProcess)
	}
}

// A whitelist entry that names a credential file is still withheld. proxy.state
// carries the proxy keys, and the whitelist says "useful", not "copyable".
func TestIncidentWhitelistCannotCopyACredentialFile(t *testing.T) {
	stateDir := incidentTestStateDir(t)
	incidentTestRestore(t)
	incidentWriteState(t, stateDir, "proxy.state", "identity_key=0123456789abcdef\n")
	incidentWriteState(t, stateDir, ".client_jwts.json", `{"direct":{"by_client_jwt":"eyJhbGciOi.eyJzdWIiOi.sig"}}`)
	incidentWriteState(t, stateDir, "proxy_url.json", `{"cache":{"10.0.0.1:8080":{"user":"u","password":"hunter2"}}}`)

	saved := incidentEvidenceStateFiles
	incidentEvidenceStateFiles = append(append([]string(nil), saved...),
		"proxy.state", ".client_jwts.json", "proxy_url.json")
	t.Cleanup(func() { incidentEvidenceStateFiles = saved })

	dir, err := captureIncidentBundle(incidentTriggerThrash, "whitelist guard", nil)
	if err != nil || dir == "" {
		t.Fatalf("capture returned (%q, %v)", dir, err)
	}
	files := readBundle(t, dir)
	inv := files["state/inventory.txt"]
	for _, name := range []string{"proxy.state", ".client_jwts.json", "proxy_url.json"} {
		if _, ok := files["state/"+name]; ok {
			t.Fatalf("a whitelisted credential file was copied: %s", name)
		}
		if lineStatus(inv, name) != "withheld:credential-file" {
			t.Fatalf("%s was not inventoried as a withheld credential:\n%s", name, inv)
		}
	}
}

// The matcher, checked against every credential-shaped file the miner writes to
// the provider state directory. A file named here is recorded by name/size/mode
// only, so a false negative is a plaintext secret in an exported bundle.
func TestIncidentSecretStateName(t *testing.T) {
	secret := []string{
		// The network and provider tokens, bare, dotted and per-slot.
		"jwt",
		".provider.jwt",
		".provider-0011223344556677.jwt",
		// The per-slot client JWT store and its snapshots: "_jwt", not ".jwt".
		".client_jwts.json",
		".client_jwts.json-20260101T000000.000000Z.bak",
		".client_jwts.json-20260101T000000.000000Z.bak.tmp",
		// Private keys and the TLS cert bundle.
		".provider.key",
		".provider.extender.key",
		".provider.extender-0011223344556677.key",
		".provider.extender.measurement.key",
		".provider.cert",
		// Proxy credentials: the config, the identity state, the lock, and the
		// URL cache, whose entries hold a plaintext user and password.
		"proxy",
		"proxy.state",
		"proxy.lock",
		"proxy_url.json",
	}
	for _, name := range secret {
		if !incidentSecretStateName(name) {
			t.Errorf("%s is credential-shaped but incidentSecretStateName says otherwise", name)
		}
	}
	// Evidence and bookkeeping must not be swept up: withholding one of these
	// would blind the bundle.
	notSecret := []string{
		"provider_state.json",
		"proxy_grades.json",
		"proxy_probe.json",
		"proxy_table_probe.json",
		"proxy_earnings.json",
		"direct_grade.json",
		"direct",
		"pending_overrides.json",
		"oom_cap.json",
		"run.marker",
		"thrash_cap.json",
		"thrash_status",
		thrashExitMarkerPrefix + "4242",
		"events.log",
		"important.log",
		"audit.json",
		"lifetime_metrics.json",
		"autopilot.jsonl",
		".hotswap_declines.json",
		".miner-run-state.json",
		"operators.yml",
		"claim-queue.json",
		"fast_auth",
	}
	for _, name := range notSecret {
		if incidentSecretStateName(name) {
			t.Errorf("%s is not a credential file but incidentSecretStateName withholds it", name)
		}
	}
	// The copy whitelist may never name a credential: the name would win and
	// the entry would be silently dead.
	for _, name := range incidentEvidenceStateFiles {
		if incidentSecretStateName(name) {
			t.Errorf("the evidence whitelist names a credential file: %s", name)
		}
	}
}

// The exit-marker scan takes this pid's marker and nothing else: a marker from
// another pid describes an exit this bundle did not witness, and a symlink is a
// path out of the state directory.
func TestIncidentThrashExitMarkerScanIsThisPidAndNoSymlink(t *testing.T) {
	stateDir := incidentTestStateDir(t)
	incidentTestRestore(t)

	pid := os.Getpid()
	mine := thrashExitMarkerPrefix + strconv.Itoa(pid)
	stale := thrashExitMarkerPrefix + strconv.Itoa(pid+1)
	linked := thrashExitMarkerPrefix + strconv.Itoa(pid+2)

	incidentWriteState(t, stateDir, mine, strconv.Itoa(pid)+"\n")
	incidentWriteState(t, stateDir, stale, "999999\n")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside-state-canary\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(stateDir, linked)); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}

	dir, err := captureIncidentBundle(incidentTriggerThrash, "restart", nil)
	if err != nil || dir == "" {
		t.Fatalf("capture returned (%q, %v)", dir, err)
	}
	files := readBundle(t, dir)
	if _, ok := files["state/"+mine]; !ok {
		t.Fatalf("this pid's marker was not copied; has %v", keysOf(files))
	}
	for _, name := range []string{stale, linked} {
		if _, ok := files["state/"+name]; ok {
			t.Fatalf("the scan copied %s, which is not this pid's evidence", name)
		}
	}
	for name, body := range files {
		if strings.Contains(body, "outside-state-canary") {
			t.Fatalf("%s followed the symlink out of the state directory", name)
		}
	}
	inv := files["state/inventory.txt"]
	if got := lineStatus(inv, linked); got != "withheld:symlink" {
		t.Fatalf("the symlinked marker is inventoried as %q:\n%s", got, inv)
	}
	if got := lineStatus(inv, stale); got != "withheld:not-evidence" {
		t.Fatalf("the stale marker is inventoried as %q:\n%s", got, inv)
	}
}

// A state file that cannot be read is not a deliberate withhold, and the
// inventory must not let the two read the same.
func TestIncidentStateReadErrorIsNotAWithhold(t *testing.T) {
	stateDir := incidentTestStateDir(t)
	incidentTestRestore(t)

	path := filepath.Join(stateDir, "provider_state.json")
	incidentWriteState(t, stateDir, "provider_state.json", `{"note":"ok"}`)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	// Root ignores the mode bits; a directory cannot be read as a file
	// (EISDIR), which is the same failure path.
	inInventory := true
	if f, err := os.Open(path); err == nil {
		f.Close()
		inInventory = false
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	incidentWriteState(t, stateDir, "unrelated.txt", "not evidence\n")

	dir, err := captureIncidentBundle(incidentTriggerFatal, "read error", nil)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("a bundle with an unreadable evidence file returned %v", err)
	}
	files := readBundle(t, dir)
	incomplete := files["incomplete.txt"]
	if !strings.Contains(incomplete, "provider_state.json could not be read") {
		t.Fatalf("incomplete.txt does not report the read failure:\n%s", incomplete)
	}
	inv := files["state/inventory.txt"]
	if got := lineStatus(inv, "unrelated.txt"); got != "withheld:not-evidence" {
		t.Fatalf("an ordinary non-evidence file is inventoried as %q:\n%s", got, inv)
	}
	if got := lineStatus(inv, "provider_state.json"); got == "withheld:not-evidence" {
		t.Fatalf("a read error is reported as a deliberate withhold:\n%s", inv)
	}
	if inInventory {
		if got := lineStatus(inv, "provider_state.json"); got != "error:read-failed" {
			t.Fatalf("an unreadable evidence file is inventoried as %q:\n%s", got, inv)
		}
	}
}

// A state file larger than the per-file cap is copied short, and the bundle
// says so instead of presenting the truncated copy as the whole file.
func TestIncidentStateTruncationIsNoted(t *testing.T) {
	stateDir := incidentTestStateDir(t)
	incidentTestRestore(t)
	incidentStateFileMaxBytes = 32

	body := strings.Repeat("0123456789abcdef", 16) // 256 bytes
	incidentWriteState(t, stateDir, "provider_state.json", body)

	dir, err := captureIncidentBundle(incidentTriggerFatal, "truncated state", nil)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("a bundle with a truncated state file returned %v", err)
	}
	files := readBundle(t, dir)
	if !strings.Contains(files["incomplete.txt"], "provider_state.json truncated at 32 bytes") {
		t.Fatalf("incomplete.txt does not report the truncation:\n%s", files["incomplete.txt"])
	}
	if got := len(files["state/provider_state.json"]); got != 32 {
		t.Fatalf("the copied state file is %d bytes, want the 32-byte cap", got)
	}
}

// incomplete.txt names the budget the bundle was actually given. The thrash
// restart asks for a shorter one than incidentBudget, and reporting the global
// number would describe a wait that never happened.
func TestIncidentBundleNamesItsOwnBudget(t *testing.T) {
	incidentTestStateDir(t)
	incidentTestRestore(t)

	root := t.TempDir()
	b, err := newIncidentBundle(root, time.Now(), incidentTriggerThrash, 1500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	b.deadline = time.Now().Add(-time.Millisecond)
	b.write("logs.txt", []byte("too late"))

	notes := strings.Join(b.notes, "\n")
	if !strings.Contains(notes, "budget of 1.5s spent before logs.txt") {
		t.Fatalf("the note blames the wrong budget:\n%s", notes)
	}
}
