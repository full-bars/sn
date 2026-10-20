package miner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- C-10 + S-03b: file-backed Workflow A -------------------------------
//
// v33 selects the curated proxy source from --proxy_file, then --file, then
// the PROXY_FILE environment variable, and records the resolved path in
// proxy.state.Source - the only switch every reader consults. These tests pin
// the miner's port of that contract: flag/env precedence, the state record,
// the file-backed desired set, and the read-back through the command reader
// (readProxySettings) that the summary/refresh/remove-dead paths use.

// writeProxyFile writes a Workflow A source file with credentialed lines.
func writeProxyFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestResolveProxyFilePrecedence pins v33's resolution order: --proxy_file
// wins, then --file, then PROXY_FILE; a leading ~ is expanded.
func TestResolveProxyFilePrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	for _, tc := range []struct {
		name string
		argv []string
		env  string
		want string
	}{
		{name: "flag wins", argv: []string{"provide", "--proxy_file=/a.txt", "--file=/b.txt"}, env: "/c.txt", want: "/a.txt"},
		{name: "file alias", argv: []string{"provide", "--file=/b.txt"}, env: "/c.txt", want: "/b.txt"},
		{name: "env fallback", argv: []string{"provide"}, env: "/c.txt", want: "/c.txt"},
		{name: "none", argv: []string{"provide"}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := parseArgsForTest(t, tc.argv)
			if tc.env == "" {
				os.Unsetenv("PROXY_FILE")
			} else {
				t.Setenv("PROXY_FILE", tc.env)
			}
			if got := resolveProxyFile(opts); got != tc.want {
				t.Fatalf("resolveProxyFile(%v, PROXY_FILE=%q) = %q, want %q", tc.argv, tc.env, got, tc.want)
			}
		})
	}

	t.Setenv("PROXY_FILE", "~/proxies.txt")
	opts := parseArgsForTest(t, []string{"provide"})
	if got, want := resolveProxyFile(opts), filepath.Join(home, "proxies.txt"); got != want {
		t.Fatalf("resolveProxyFile(~ expansion) = %q, want %q", got, want)
	}
}

// TestWorkflowASelectedThroughStateSource is the S-03b wiring proof: starting
// the manager with a proxy file records proxy.state.Source, builds the desired
// set from that file (not the internal config), and tags the entries "file".
func TestWorkflowASelectedThroughStateSource(t *testing.T) {
	withTempHome(t)

	sourcePath := filepath.Join(t.TempDir(), "proxies.txt")
	writeProxyFile(t, sourcePath, "1.1.1.1:1080:alice:secret", "2.2.2.2:1080:bob:secret")

	// The internal config carries a different curated entry: Workflow A must
	// not read it while a file source is selected.
	writeProxyConfig(&ProxyConfig{Servers: map[string]string{"9.9.9.9:1080": ""}})

	cfg := defaultProxyManagerConfig()
	cfg.ProxyFile = sourcePath
	m := newProxyManager(cfg)
	launch, _, writeState, err := m.desired(true)
	if err != nil {
		t.Fatal(err)
	}
	if !writeState {
		t.Fatal("selecting a source file must mark proxy.state for writing")
	}
	// desired() reports the write; the start path performs it (see start()).
	if err := writeProxyState(m.state); err != nil {
		t.Fatal(err)
	}

	var keys []string
	for _, s := range launch {
		keys = append(keys, s.Key())
	}
	if len(keys) != 2 || !strings.Contains(strings.Join(keys, ","), "1.1.1.1:1080") || !strings.Contains(strings.Join(keys, ","), "2.2.2.2:1080") {
		t.Fatalf("launch = %v, want exactly the two file entries", keys)
	}
	for _, k := range keys {
		if strings.Contains(k, "9.9.9.9:1080") {
			t.Fatalf("the internal config entry leaked into a file-backed desired set: %v", keys)
		}
	}

	state, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Source != sourcePath {
		t.Fatalf("proxy.state.Source = %q, want %q", state.Source, sourcePath)
	}
	for _, k := range keys {
		if got := state.Proxies[k].Source; got != "file" {
			t.Fatalf("state entry %q source = %q, want file", k, got)
		}
	}

	// The command reader (summary/refresh/remove-dead) must select the same
	// source from the recorded state alone, with no flags.
	readBack := proxyManagerReadSettings()
	if len(readBack) != 2 {
		t.Fatalf("readProxySettings() = %d entries, want the 2 file entries", len(readBack))
	}
	for _, s := range readBack {
		if strings.Contains(s.Address, "9.9.9.9") {
			t.Fatalf("readProxySettings() fell back to the internal config: %v", readBack)
		}
	}
}

// TestWorkflowBClearsStaleSource pins the other half of v33's write: starting
// without a file clears a stale proxy.state.Source and selects the internal
// config, whose entries are tagged "internal".
func TestWorkflowBClearsStaleSource(t *testing.T) {
	withTempHome(t)

	if err := writeProxyState(&ProxyState{Source: "/stale/proxies.txt", Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	writeProxyConfig(&ProxyConfig{Servers: map[string]string{"3.3.3.3:1080": ""}})

	m := newProxyManager(defaultProxyManagerConfig())
	launch, _, writeState, err := m.desired(true)
	if err != nil {
		t.Fatal(err)
	}
	if !writeState {
		t.Fatal("clearing a stale source must mark proxy.state for writing")
	}
	if err := writeProxyState(m.state); err != nil {
		t.Fatal(err)
	}
	if len(launch) != 1 || launch[0].Address != "3.3.3.3:1080" {
		t.Fatalf("launch = %v, want the internal config entry", launch)
	}
	state, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Source != "" {
		t.Fatalf("proxy.state.Source = %q, want cleared", state.Source)
	}
	if got := state.Proxies["3.3.3.3:1080"].Source; got != "internal" {
		t.Fatalf("internal entry source tag = %q, want internal", got)
	}
}

// TestFileBackedStartRefusesEmptySource mirrors v33's fatal refusal when the
// selected file yields no usable proxies: a start must not silently provide
// with a curated pool of zero.
func TestFileBackedStartRefusesEmptySource(t *testing.T) {
	withTempHome(t)

	sourcePath := filepath.Join(t.TempDir(), "proxies.txt")
	// Comment-only content: readable, but no valid credentialed entries.
	writeProxyFile(t, sourcePath, "# just a comment")

	cfg := defaultProxyManagerConfig()
	cfg.ProxyFile = sourcePath
	m := newProxyManager(cfg)
	if _, _, _, err := m.desired(true); err == nil {
		t.Fatal("an empty file-backed source must refuse the start")
	}
}

// TestProxyProvideProxyFilePatternsParse proves the provide grammar now
// accepts --proxy_file and --file (they were a usage error before C-10).
func TestProxyProvideProxyFilePatternsParse(t *testing.T) {
	for _, argv := range [][]string{
		{"provide", "--proxy_file=/synthetic/proxies.txt"},
		{"provide", "--file=/synthetic/proxies.txt"},
		{"auth-provide", "synthetic-code", "--proxy_file=/synthetic/proxies.txt", "--file=/synthetic/proxies.txt"},
	} {
		opts := parseArgsForTest(t, argv)
		if got := resolveProxyFile(opts); got != "/synthetic/proxies.txt" {
			t.Fatalf("parse %v: resolveProxyFile = %q", argv, got)
		}
	}
}

// TestProxyProvideProxyFileEnvOnly mirrors the Docker/Pelican path: the
// environment variable is the only knob.
func TestProxyProvideProxyFileEnvOnly(t *testing.T) {
	t.Setenv("PROXY_FILE", "/synthetic/from-env.txt")
	opts := parseArgsForTest(t, []string{"provide"})
	if got := resolveProxyFile(opts); got != "/synthetic/from-env.txt" {
		t.Fatalf("PROXY_FILE-only resolution = %q", got)
	}
}

// TestProxyManagerStartRecordsFileSource drives the real start path (not just
// desired) and proves the source tagging follows the selected workflow.
func TestProxyManagerStartRecordsFileSource(t *testing.T) {
	withTempHome(t)

	sourcePath := filepath.Join(t.TempDir(), "proxies.txt")
	writeProxyFile(t, sourcePath, "4.4.4.4:1080:carol:secret")

	cfg := defaultProxyManagerConfig()
	cfg.ProxyFile = sourcePath
	m := newProxyManager(cfg)
	launch, _, err := m.start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer m.stop()
	if len(launch) != 1 || launch[0].Address != "4.4.4.4:1080" {
		t.Fatalf("start launch = %v, want the file entry", launch)
	}
	if got := m.sourceOf[launch[0].Key()]; got != "file" {
		t.Fatalf("start sourceOf tag = %q, want file", got)
	}
	state, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Source != sourcePath {
		t.Fatalf("proxy.state.Source = %q, want %q", state.Source, sourcePath)
	}
}
