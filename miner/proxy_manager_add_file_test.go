package miner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// proxy_manager_add_file_test.go pins the Workflow A add routing the paste
// hand-off depends on: when proxy.state selects a source file (C-10), `proxy
// add` appends to that file instead of the internal config a file-backed
// reload discards.

// TestProxyAddFileBackedAppendsAndDedupes drives the real `proxy add` entry
// point with a file-backed state: the new line is appended, an existing line
// is not duplicated, comments and existing content survive, and the internal
// config is left untouched.
func TestProxyAddFileBackedAppendsAndDedupes(t *testing.T) {
	withTempHome(t)

	sourcePath := filepath.Join(t.TempDir(), "proxies.txt")
	if err := os.WriteFile(sourcePath, []byte("# operator note\n1.1.1.1:1080:alice:secret\n\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// The state selects the file-backed workflow.
	if err := writeProxyState(&ProxyState{Source: sourcePath, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	// The internal config carries a different entry: a file-backed add must
	// not write to it.
	writeProxyConfig(&ProxyConfig{Servers: map[string]string{"9.9.9.9:1080": ""}})

	// The paste hand-off shape: normalized lines in a temp file, added with -f.
	handoff := filepath.Join(t.TempDir(), "paste.txt")
	if err := os.WriteFile(handoff, []byte("1.1.1.1:1080:alice:secret\n2.2.2.2:1080:bob:secret\n"), 0600); err != nil {
		t.Fatal(err)
	}

	opts := parseArgsForTest(t, []string{"proxy", "add", "--proxy_file=" + handoff, "-f"})
	proxyAdd(opts)

	b, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	if !strings.Contains(content, "# operator note") {
		t.Fatalf("comment line was dropped:\n%s", content)
	}
	if got := strings.Count(content, "1.1.1.1:1080:alice:secret"); got != 1 {
		t.Fatalf("existing line appears %d times, want 1:\n%s", got, content)
	}
	if got := strings.Count(content, "2.2.2.2:1080:bob:secret"); got != 1 {
		t.Fatalf("added line appears %d times, want 1:\n%s", got, content)
	}

	// The internal config must not have gained the pasted entries.
	cfg := readProxyConfig()
	if len(cfg.Servers) != 1 || cfg.Servers["9.9.9.9:1080"] != "" {
		t.Fatalf("file-backed add touched the internal config: %v", cfg.Servers)
	}
}

// TestProxyAddFileBackedNoDuplicatesOnly: a second add of only known lines
// leaves the file byte-identical and reports nothing added.
func TestProxyAddFileBackedNoDuplicatesOnly(t *testing.T) {
	withTempHome(t)

	sourcePath := filepath.Join(t.TempDir(), "proxies.txt")
	original := []byte("1.1.1.1:1080:alice:secret\n")
	if err := os.WriteFile(sourcePath, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{Source: sourcePath, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}

	handoff := filepath.Join(t.TempDir(), "paste.txt")
	if err := os.WriteFile(handoff, []byte("1.1.1.1:1080:alice:secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts := parseArgsForTest(t, []string{"proxy", "add", "--proxy_file=" + handoff, "-f"})

	out := captureStdout(t, func() { proxyAdd(opts) })
	if !strings.Contains(out, "no new proxies to add") {
		t.Fatalf("expected the no-op message, got:\n%s", out)
	}
	b, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(original) {
		t.Fatalf("duplicate-only add rewrote the file:\n%q", b)
	}
}

// TestProxyAddInternalWhenNoSourceSelected guards the other half: a state with
// no Source keeps the internal-config add path.
func TestProxyAddInternalWhenNoSourceSelected(t *testing.T) {
	withTempHome(t)

	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	opts := parseArgsForTest(t, []string{"proxy", "add", "3.3.3.3:1080:carol:secret", "-f"})
	proxyAdd(opts)

	cfg := readProxyConfig()
	if _, ok := cfg.Servers["3.3.3.3:1080:carol:secret"]; !ok {
		t.Fatalf("internal add did not land in the config: %v", cfg.Servers)
	}
}
