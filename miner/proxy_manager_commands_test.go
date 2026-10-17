package miner

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// authSettingsFor builds a credentialed proxy identity (v33 test helper).
func authSettingsFor(addr, user string) *connect.ProxySettings {
	return &connect.ProxySettings{Network: "tcp", Address: addr, Auth: &proxy.Auth{User: user, Password: "p"}}
}

// proxySet indexes a candidate slice by address for set-comparison.
func proxySet(items []removedProxy) map[string]bool {
	m := map[string]bool{}
	for _, rp := range items {
		m[rp.addr] = true
	}
	return m
}

// assertCandidatesEqual asserts the collected candidate list contains exactly
// the want addresses (order-independent).
func assertCandidatesEqual(t *testing.T, got []removedProxy, want ...string) {
	t.Helper()
	g := proxySet(got)
	w := map[string]bool{}
	for _, s := range want {
		w[s] = true
	}
	var gotAddrs []string
	for _, rp := range got {
		gotAddrs = append(gotAddrs, rp.addr)
	}
	sort.Strings(gotAddrs)
	for _, s := range want {
		if !g[s] {
			t.Errorf("missing candidate %q (want %v, got %v)", s, want, gotAddrs)
		}
	}
	for s := range g {
		if !w[s] {
			t.Errorf("unexpected candidate %q (want %v, got %v)", s, want, gotAddrs)
		}
	}
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	defer func() { os.Stdout = old }()

	fn()
	os.Stdout = old
	w.Close()
	out := <-done
	r.Close()
	return out
}

// feedStdin replaces os.Stdin with a pipe carrying input for one command.
func feedStdin(t *testing.T, input string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		r.Close()
	})
}

// ---- grammar ----

// TestProxyCommandGrammar parses every new subcommand through the same docopt
// usage the CLI uses, and pins the option binding each handler reads.
func TestProxyCommandGrammar(t *testing.T) {
	cases := []struct {
		argv   []string
		assert func(t *testing.T, opts docopt.Opts)
	}{
		{
			argv: []string{"proxy", "add-source", "https://example.invalid/list.txt"},
			assert: func(t *testing.T, opts docopt.Opts) {
				if v, _ := opts.String("<url>"); v != "https://example.invalid/list.txt" {
					t.Errorf("<url> = %q", v)
				}
				if b, _ := opts.Bool("add-source"); !b {
					t.Error("add-source not set")
				}
			},
		},
		{
			argv: []string{"proxy", "remove-source", "https://example.invalid/list.txt"},
			assert: func(t *testing.T, opts docopt.Opts) {
				if b, _ := opts.Bool("remove-source"); !b {
					t.Error("remove-source not set")
				}
			},
		},
		{
			argv: []string{"proxy", "refresh", "--force"},
			assert: func(t *testing.T, opts docopt.Opts) {
				if b, _ := opts.Bool("refresh"); !b {
					t.Error("refresh not set")
				}
				if b, _ := opts.Bool("--force"); !b {
					t.Error("--force not set")
				}
			},
		},
		{
			argv: []string{"proxy", "trim", "25", "--preview"},
			assert: func(t *testing.T, opts docopt.Opts) {
				if v, _ := opts.String("<count>"); v != "25" {
					t.Errorf("<count> = %q", v)
				}
				if b, _ := opts.Bool("--preview"); !b {
					t.Error("--preview not set")
				}
			},
		},
		{
			argv: []string{"proxy", "summary"},
			assert: func(t *testing.T, opts docopt.Opts) {
				if b, _ := opts.Bool("summary"); !b {
					t.Error("summary not set")
				}
			},
		},
		{
			argv: []string{"proxy", "exclude", "gw.example", "--remove"},
			assert: func(t *testing.T, opts docopt.Opts) {
				if v, _ := opts.String("<pattern>"); v != "gw.example" {
					t.Errorf("<pattern> = %q", v)
				}
				if b, _ := opts.Bool("--remove"); !b {
					t.Error("--remove not set")
				}
			},
		},
		{
			argv: []string{"proxy", "exclude"},
			assert: func(t *testing.T, opts docopt.Opts) {
				if v, _ := opts.String("<pattern>"); v != "" {
					t.Errorf("<pattern> = %q, want empty for the list form", v)
				}
			},
		},
		{
			argv: []string{"proxy", "remove-dead", "--degraded", "--source=url", "--yes"},
			assert: func(t *testing.T, opts docopt.Opts) {
				if b, _ := opts.Bool("--degraded"); !b {
					t.Error("--degraded not set")
				}
				if v, _ := opts.String("--source"); v != "url" {
					t.Errorf("--source = %q", v)
				}
				if b, _ := opts.Bool("--yes"); !b {
					t.Error("--yes not set")
				}
			},
		},
		{
			argv: []string{"proxy", "remove-dead", "--auth-failures=100", "--preview"},
			assert: func(t *testing.T, opts docopt.Opts) {
				if n, _ := opts.Int("--auth-failures"); n != 100 {
					t.Errorf("--auth-failures = %d", n)
				}
				if b, _ := opts.Bool("--preview"); !b {
					t.Error("--preview not set")
				}
			},
		},
		{
			argv: []string{"proxy", "activity"},
			assert: func(t *testing.T, opts docopt.Opts) {
				if b, _ := opts.Bool("activity"); !b {
					t.Error("activity not set")
				}
			},
		},
		// The existing commands keep their bindings.
		{
			argv: []string{"proxy", "add", "key@1.2.3.4:1080"},
			assert: func(t *testing.T, opts docopt.Opts) {
				if b, _ := opts.Bool("add"); !b {
					t.Error("add not set")
				}
				if b, _ := opts.Bool("add-source"); b {
					t.Error("add-source must not be set for 'proxy add'")
				}
			},
		},
		{
			argv: []string{"proxy", "remove", "--", "1.2.3.4:1080"},
			assert: func(t *testing.T, opts docopt.Opts) {
				if b, _ := opts.Bool("remove"); !b {
					t.Error("remove not set")
				}
				if b, _ := opts.Bool("remove-source"); b {
					t.Error("remove-source must not be set for 'proxy remove'")
				}
			},
		},
	}

	for _, c := range cases {
		t.Run(strings.Join(c.argv, " "), func(t *testing.T) {
			parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
			opts, err := parser.ParseArgs(mainUsage(), c.argv, "test")
			if err != nil {
				t.Fatalf("usage does not accept %v: %v", c.argv, err)
			}
			c.assert(t, opts)
		})
	}
}

// TestProxyRemoveDeadDurationFlagMatchesV33 pins a v33 grammar quirk the port
// keeps deliberately: docopt-go (the same version v33 pins) supports the bare
// `--degraded` flag of `[--degraded[=<duration>]]`, but rejects the
// `--degraded=<duration>` spelling. v33's handler therefore always takes the
// 24h default, and so does this port.
func TestProxyRemoveDeadDurationFlagMatchesV33(t *testing.T) {
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}

	opts, err := parser.ParseArgs(mainUsage(), []string{"proxy", "remove-dead", "--degraded"}, "test")
	if err != nil {
		t.Fatalf("bare --degraded must parse: %v", err)
	}
	if b, _ := opts.Bool("--degraded"); !b {
		t.Fatal("bare --degraded must set the flag")
	}
	if _, err := parser.ParseArgs(mainUsage(), []string{"proxy", "remove-dead", "--degraded=48h"}, "test"); err == nil {
		t.Fatal("docopt-go rejects --degraded=<duration>; v33 behaves the same way, so the port must too")
	}
}

// ---- add-source / remove-source ----

func TestProxyAddSource_PersistsAndFetches(t *testing.T) {
	withTempHome(t)
	allowLoopbackForTest(t)

	addr1, cleanup1 := listenSocks5Once(t)
	defer cleanup1()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(addr1 + "\n"))
	}))
	defer srv.Close()

	out := captureStdout(t, func() {
		proxyAddSource(docopt.Opts{"<url>": srv.URL})
	})

	if !strings.Contains(out, "added source: "+srv.URL) || !strings.Contains(out, "done.") {
		t.Fatalf("unexpected output: %q", out)
	}

	state, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Sources) != 1 || state.Sources[0] != srv.URL {
		t.Fatalf("sources = %v, want [%s]", state.Sources, srv.URL)
	}
	if _, ok := state.Cache[addr1]; !ok {
		t.Fatalf("fetched address %s missing from the cache: %v", addr1, state.Cache)
	}
}

func TestProxyAddSource_AlreadyAddedIsANoOp(t *testing.T) {
	withTempHome(t)

	if err := writeProxyURLState(&ProxyURLState{Sources: []string{"https://example.invalid/a"}, Cache: map[string]ProxyURLEntry{}}); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		proxyAddSource(docopt.Opts{"<url>": "https://example.invalid/a"})
	})
	if !strings.Contains(out, "source already added: https://example.invalid/a") {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestProxyRemoveSource_KeepsCachedAddresses(t *testing.T) {
	withTempHome(t)

	if err := writeProxyURLState(&ProxyURLState{
		Sources: []string{"https://example.invalid/a", "https://example.invalid/b"},
		Cache:   map[string]ProxyURLEntry{"1.2.3.4:1080": {}},
	}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		proxyRemoveSource(docopt.Opts{"<url>": "https://example.invalid/a"})
	})
	if !strings.Contains(out, "removed source: https://example.invalid/a") {
		t.Fatalf("unexpected output: %q", out)
	}
	if !strings.Contains(out, "previously fetched proxies from this source remain running") {
		t.Fatalf("missing the cache-retention note: %q", out)
	}

	state, err := readProxyURLState()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(state.Sources, []string{"https://example.invalid/b"}) {
		t.Fatalf("sources = %v", state.Sources)
	}
	if _, ok := state.Cache["1.2.3.4:1080"]; !ok {
		t.Fatal("cached address must survive a source removal")
	}
}

func TestProxyRemoveSource_UnknownSource(t *testing.T) {
	withTempHome(t)

	if err := writeProxyURLState(&ProxyURLState{Sources: []string{"https://example.invalid/a"}, Cache: map[string]ProxyURLEntry{}}); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		proxyRemoveSource(docopt.Opts{"<url>": "https://example.invalid/missing"})
	})
	if !strings.Contains(out, "source not found: https://example.invalid/missing") {
		t.Fatalf("unexpected output: %q", out)
	}
}

// ---- exclude ----

func TestProxyExclude_AddListRemove(t *testing.T) {
	withTempHome(t)

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{}}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		proxyExclude(docopt.Opts{"<pattern>": "gw.example"})
	})
	if !strings.Contains(out, `added exclude pattern "gw.example"`) {
		t.Fatalf("add: %q", out)
	}

	state, _ := readProxyURLState()
	if len(state.ExcludePatterns) != 1 || state.ExcludePatterns[0] != "gw.example" {
		t.Fatalf("patterns = %v", state.ExcludePatterns)
	}

	out = captureStdout(t, func() {
		proxyExclude(docopt.Opts{})
	})
	if !strings.Contains(out, "1 exclude patterns (URL fetches skip matching hosts):") || !strings.Contains(out, "    gw.example") {
		t.Fatalf("list: %q", out)
	}

	out = captureStdout(t, func() {
		proxyExclude(docopt.Opts{"<pattern>": "gw.example", "--remove": true})
	})
	if !strings.Contains(out, `removed exclude pattern "gw.example"`) {
		t.Fatalf("remove: %q", out)
	}
	state, _ = readProxyURLState()
	if len(state.ExcludePatterns) != 0 {
		t.Fatalf("patterns after remove = %v", state.ExcludePatterns)
	}

	out = captureStdout(t, func() {
		proxyExclude(docopt.Opts{"<pattern>": "gw.example", "--remove": true})
	})
	if !strings.Contains(out, `pattern "gw.example" is not in the exclude list`) {
		t.Fatalf("unknown remove: %q", out)
	}
}

func TestProxyExclude_EmptyListSaysSo(t *testing.T) {
	withTempHome(t)

	out := captureStdout(t, func() {
		proxyExclude(docopt.Opts{})
	})
	if !strings.Contains(out, "no exclude patterns set") {
		t.Fatalf("unexpected output: %q", out)
	}
}

// ---- refresh ----

// TestPlanProxyRefresh_CredentialedProxyInSyncIsNotAChange: a tracked
// credentialed proxy that is still in the desired list must be neither
// "removed" nor "added". Diffing by bare address listed every one as both, and
// the operator confirmed a prompt full of raw identity keys.
func TestPlanProxyRefresh_CredentialedProxyInSyncIsNotAChange(t *testing.T) {
	desired := []*connect.ProxySettings{authSettingsFor("gw.example:1080", "u1"), authSettingsFor("gw.example:1080", "u2")}
	current := map[string]ProxyEntry{
		identityKey("gw.example:1080", "u1"): {ID: 1, Health: "up"},
		identityKey("gw.example:1080", "u2"): {ID: 2, Health: "up"},
	}
	added, removed := planProxyRefresh(desired, current)
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("in-sync list must produce no changes, got added=%v removed=%v", added, removed)
	}
}

// TestPlanProxyRefresh_AccountAtSharedGatewayChanges: one account leaving a
// shared gateway removes exactly that identity, and a new account is added by
// its own key, sorted for a stable prompt.
func TestPlanProxyRefresh_AccountAtSharedGatewayChanges(t *testing.T) {
	desired := []*connect.ProxySettings{authSettingsFor("gw.example:1080", "u2"), authSettingsFor("gw.example:1080", "u4"), authSettingsFor("gw.example:1080", "u3")}
	current := map[string]ProxyEntry{
		identityKey("gw.example:1080", "u1"): {ID: 1, Health: "dead"},
		identityKey("gw.example:1080", "u2"): {ID: 2, Health: "up"},
	}
	added, removed := planProxyRefresh(desired, current)

	wantAdded := []string{identityKey("gw.example:1080", "u3"), identityKey("gw.example:1080", "u4")}
	if !reflect.DeepEqual(added, wantAdded) {
		t.Fatalf("added=%q, want %q", added, wantAdded)
	}
	if len(removed) != 1 || removed[0].key != identityKey("gw.example:1080", "u1") || removed[0].entry.Health != "dead" {
		t.Fatalf("removed=%+v, want only the u1 account", removed)
	}
}

// TestPlanProxyRefresh_BareAddressAndHealthClassification: unauthenticated
// proxies keep their bare-address key and an empty health is classified.
func TestPlanProxyRefresh_BareAddressAndHealthClassification(t *testing.T) {
	desired := []*connect.ProxySettings{{Network: "tcp", Address: "10.0.0.1:1080"}}
	current := map[string]ProxyEntry{"10.0.0.2:1080": {ID: 2}}
	added, removed := planProxyRefresh(desired, current)
	if !reflect.DeepEqual(added, []string{"10.0.0.1:1080"}) {
		t.Fatalf("added=%q", added)
	}
	if len(removed) != 1 || removed[0].key != "10.0.0.2:1080" || removed[0].entry.Health != "starting" {
		t.Fatalf("removed=%+v, want the bare-address proxy classified starting", removed)
	}
}

// TestPlanProxyRefresh_RepeatedIdentityIsAddedOnce: a source that lists one
// identity twice adds it once.
func TestPlanProxyRefresh_RepeatedIdentityIsAddedOnce(t *testing.T) {
	desired := []*connect.ProxySettings{authSettingsFor("gw.example:1080", "u1"), authSettingsFor("gw.example:1080", "u1")}
	added, _ := planProxyRefresh(desired, map[string]ProxyEntry{})
	if want := []string{identityKey("gw.example:1080", "u1")}; !reflect.DeepEqual(added, want) {
		t.Fatalf("added=%q, want %q", added, want)
	}
}

func TestProxyRefresh_UpToDateShortCircuits(t *testing.T) {
	withTempHome(t)

	settings := authSettingsFor("gw.example:1080", "u1")
	if err := writeProxyState(&ProxyState{
		StartedAt: time.Now().Add(-9 * time.Hour),
		Proxies:   map[string]ProxyEntry{settings.Key(): {ID: 1, Health: "up"}},
	}); err != nil {
		t.Fatal(err)
	}
	writeProxyConfig(&ProxyConfig{
		Auths:   map[string]*ProxyAuth{"k": {User: "u1", Password: "p"}},
		Servers: map[string]string{"gw.example:1080": "k"},
	})

	reloadPath, _ := proxyReloadPath()
	before, _ := readReloadSeq(reloadPath)

	out := captureStdout(t, func() {
		proxyRefresh(docopt.Opts{})
	})
	if !strings.Contains(out, "proxy list is already up to date. Nothing to do.") {
		t.Fatalf("unexpected output: %q", out)
	}
	after, _ := readReloadSeq(reloadPath)
	if after != before {
		t.Fatalf("an up-to-date refresh must not write the reload trigger (%d -> %d)", before, after)
	}
}

func TestProxyRefresh_ConfirmedDiffWritesReloadTrigger(t *testing.T) {
	withTempHome(t)
	feedStdin(t, "y\n")

	if err := writeProxyState(&ProxyState{
		StartedAt: time.Now().Add(-9 * time.Hour),
		Proxies:   map[string]ProxyEntry{identityKey("gw.example:1080", "u2"): {ID: 1, Health: "dead"}},
	}); err != nil {
		t.Fatal(err)
	}
	writeProxyConfig(&ProxyConfig{
		Auths:   map[string]*ProxyAuth{"k": {User: "u1", Password: "p"}},
		Servers: map[string]string{"gw.example:1080": "k"},
	})

	reloadPath, _ := proxyReloadPath()
	before, _ := readReloadSeq(reloadPath)

	out := captureStdout(t, func() {
		proxyRefresh(docopt.Opts{})
	})
	if !strings.Contains(out, "proxy refresh: 1 proxies will be removed, 1 will be added.") {
		t.Fatalf("diff header missing: %q", out)
	}
	if !strings.Contains(out, "  Removing:") || !strings.Contains(out, "  Adding:") {
		t.Fatalf("diff body missing: %q", out)
	}
	if !strings.Contains(out, "Reload triggered. Provider will apply changes within 2 seconds.") {
		t.Fatalf("trigger line missing: %q", out)
	}
	if after, _ := readReloadSeq(reloadPath); after != before+1 {
		t.Fatalf("reload trigger not written: %d -> %d", before, after)
	}
}

// ---- trim ----

func TestProxyTrim_ClearsCapWithoutARunningProvider(t *testing.T) {
	withTempHome(t)

	if err := writeTrimTarget(9); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		proxyTrim(docopt.Opts{"<count>": "off"})
	})
	if !strings.Contains(out, "proxy trim: cap cleared") {
		t.Fatalf("unexpected output: %q", out)
	}
	if cap, _ := readTrimTarget(); cap != 0 {
		t.Fatalf("cap = %d, want cleared", cap)
	}

	out = captureStdout(t, func() {
		proxyTrim(docopt.Opts{"<count>": "off", "--preview": true})
	})
	if !strings.Contains(out, "preview: would clear the proxy trim cap") {
		t.Fatalf("preview of a clear: %q", out)
	}
}

func TestProxyTrim_SetsCapAndTriggersReload(t *testing.T) {
	withTempHome(t)

	if err := writeProxyState(&ProxyState{
		StartedAt: time.Now().Add(-9 * time.Hour),
		Proxies:   map[string]ProxyEntry{},
	}); err != nil {
		t.Fatal(err)
	}
	reloadPath, _ := proxyReloadPath()
	before, _ := readReloadSeq(reloadPath)

	out := captureStdout(t, func() {
		proxyTrim(docopt.Opts{"<count>": "42"})
	})
	if !strings.Contains(out, "proxy trim: cap set to 42 running proxies; reloading to shed the worst-graded above it") {
		t.Fatalf("unexpected output: %q", out)
	}
	if cap, _ := readTrimTarget(); cap != 42 {
		t.Fatalf("cap = %d, want 42", cap)
	}
	if after, _ := readReloadSeq(reloadPath); after != before+1 {
		t.Fatalf("reload trigger not written: %d -> %d", before, after)
	}
}

func TestProxyTrim_InvalidCount(t *testing.T) {
	withTempHome(t)

	out := captureStdout(t, func() {
		proxyTrim(docopt.Opts{"<count>": "many"})
	})
	if !strings.Contains(out, `invalid proxy count "many" (use a number or 'off')`) {
		t.Fatalf("unexpected output: %q", out)
	}
}

// TestProxyTrim_PreviewNeedsTheRunningProvider: the preview must come from the
// provider's control socket (a CLI process has no running set of its own), so
// without one the command says so instead of reporting "nothing to shed".
func TestProxyTrim_PreviewNeedsTheRunningProvider(t *testing.T) {
	withTempHome(t)

	if err := writeProxyState(&ProxyState{
		StartedAt: time.Now().Add(-9 * time.Hour),
		Proxies:   map[string]ProxyEntry{},
	}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		proxyTrim(docopt.Opts{"<count>": "3", "--preview": true})
	})
	if !strings.Contains(out, "preview unavailable:") {
		t.Fatalf("unexpected output: %q", out)
	}
	if cap, _ := readTrimTarget(); cap != 0 {
		t.Fatalf("a preview must not write the cap, got %d", cap)
	}
}

// ---- summary / activity ----

func TestProxySummary_ReportsSourcesAndCache(t *testing.T) {
	home := withTempHome(t)
	healthDir := filepath.Join(home, "health")
	t.Setenv("URNETWORK_PROXY_HEALTH_DIR", healthDir)
	if err := os.MkdirAll(healthDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(healthDir, "proxy_health.state"),
		[]byte(" Up: 3 | Down: 1 | Dead: 2 | Degraded: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := writeProxyState(&ProxyState{
		StartedAt: time.Now().Add(-2 * time.Hour),
		Proxies: map[string]ProxyEntry{
			"1.1.1.1:1080":                   {ID: 1, Health: "up", Source: "file"},
			identityKey("2.2.2.2:1080", "u"): {ID: 2, Health: "up", Source: "url"},
			"3.3.3.3:1080":                   {ID: 3},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyURLState(&ProxyURLState{
		Sources:         []string{"https://example.invalid/list"},
		Cache:           map[string]ProxyURLEntry{"2.2.2.2:1080": {}},
		Blacklist:       map[string]time.Time{"9.9.9.9:1080": time.Now()},
		ExcludePatterns: []string{"gw.example"},
	}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { proxySummary() })

	for _, want := range []string{
		" PROXY SUMMARY",
		"  Total proxies:      3",
		"  Up:                 3",
		"  Dead:               2",
		"  Degraded:           1",
		" --- Sources ---",
		"  URL proxies:        1",
		"  Internal proxies:   1",
		"  Source URLs:        1",
		"  Cached addresses:   1",
		"  Blacklisted:        1",
		"  Exclude patterns:   gw.example",
		"    https://example.invalid/list",
		"  Proxy state file:",
		"  URL state:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q\n%s", want, out)
		}
	}
}

func TestProxyActivity_Snapshot(t *testing.T) {
	home := withTempHome(t)
	healthDir := filepath.Join(home, "health")
	t.Setenv("URNETWORK_PROXY_HEALTH_DIR", healthDir)
	if err := os.MkdirAll(healthDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(healthDir, "proxy_health.state"),
		[]byte(" Up: 2 | Down: 0 | Dead: 0 | Degraded: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(healthDir, "proxy_traffic.state"),
		[]byte("| proxy[1] | 1.2.3.4:1080 | 2 | 5m | 10 MB / 20 MB | 100 MB / 200 MB |\n"), 0600); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { proxyActivity() })
	for _, want := range []string{
		"Proxy Activity Monitor",
		"Press Ctrl+C to exit",
		"Up: 2 | Degraded: 1",
		"| proxy[1] | 1.2.3.4:1080 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("activity missing %q\n%s", want, out)
		}
	}
}

// ---- remove-dead ----

// TestCollectRemoveDeadCandidates exercises the pure decision core of `proxy
// remove-dead`. Because it is a pure function the tests feed it a state with
// explicit Health verdicts (the output of the real health prober) and a
// backdated uptime, simulating a settled ~70-min-old provider with no wait.
func TestCollectRemoveDeadCandidates(t *testing.T) {
	now := time.Now()
	downOld := now.Add(-48 * time.Hour).Format(time.RFC3339)
	downRecent := now.Add(-12 * time.Hour).Format(time.RFC3339)

	cases := []struct {
		name     string
		stateSrc string
		proxies  map[string]ProxyEntry
		opts     removeDeadOptions
		uptime   time.Duration
		wantDead []string
		wantIna  []string
		wantDeg  []string
		wantAuth []string
	}{
		{
			name: "dead+inactive+up",
			proxies: map[string]ProxyEntry{
				"d": {ID: 1, Health: "dead", Source: "file"},
				"i": {ID: 2, Health: "inactive", Source: "file"},
				"u": {ID: 3, Health: "up", Source: "file"},
			},
			wantDead: []string{"d"},
			wantIna:  []string{"i"},
		},
		{
			name:     "source-filter picks url only; untagged resolves via state.Source",
			stateSrc: "proxy.txt",
			proxies: map[string]ProxyEntry{
				"f1": {ID: 1, Health: "dead", Source: "file"},
				"u1": {ID: 2, Health: "dead", Source: "url"},
				"un": {ID: 3, Health: "dead"}, // untagged -> resolves to "file" (state.Source set)
			},
			opts:     removeDeadOptions{sourceFilter: "url"},
			wantDead: []string{"u1"},
		},
		{
			name:     "untagged resolves to internal when no state.Source",
			stateSrc: "",
			proxies: map[string]ProxyEntry{
				"un": {ID: 1, Health: "dead"},
			},
			opts:     removeDeadOptions{sourceFilter: "internal"},
			wantDead: []string{"un"},
		},
		{
			name: "degraded honors down-since window; recent kept",
			proxies: map[string]ProxyEntry{
				"old": {ID: 1, Health: "offline", Source: "file", DownSince: downOld},
				"new": {ID: 2, Health: "offline", Source: "file", DownSince: downRecent},
			},
			opts:    removeDeadOptions{degradedDur: 24 * time.Hour},
			uptime:  2 * time.Hour,
			wantDeg: []string{"old"},
		},
		{
			name: "degraded: without --degraded (degradedDur=0) offline proxies are NOT degraded",
			proxies: map[string]ProxyEntry{
				"old": {ID: 1, Health: "offline", Source: "file", DownSince: downOld},
			},
			opts:   removeDeadOptions{degradedDur: 0},
			uptime: 2 * time.Hour,
			// Old down-since but NO --degraded -> must NOT be collected (matches auto path).
			wantDeg: []string{},
		},
		{
			name: "degradedDur>0 but recent offline still auth-fails if threshold met",
			proxies: map[string]ProxyEntry{
				"recent_off_hi": {ID: 1, Health: "offline", Source: "file", DownSince: downRecent, AuthFailures: 500},
			},
			opts:     removeDeadOptions{degradedDur: 24 * time.Hour, authFailMin: 100},
			uptime:   48 * time.Hour,            // days=2, threshold=200 -> 500 >= 200 auth-fails
			wantDeg:  []string{},                // not old enough to be degraded
			wantAuth: []string{"recent_off_hi"}, // still auth-fail eligible (deliberate)
		},
		{
			name: "long_offline and recently_offline also collected as degraded when old enough",
			proxies: map[string]ProxyEntry{
				"lo": {ID: 1, Health: "long_offline", Source: "file", DownSince: downOld},
				"ro": {ID: 2, Health: "recently_offline", Source: "file", DownSince: downOld},
			},
			opts:    removeDeadOptions{degradedDur: 24 * time.Hour},
			uptime:  2 * time.Hour,
			wantDeg: []string{"lo", "ro"},
		},
		{
			name: "unparsable down-since is fail-closed (not removed)",
			proxies: map[string]ProxyEntry{
				"bad": {ID: 1, Health: "offline", Source: "file", DownSince: "not-a-time"},
			},
			opts:    removeDeadOptions{degradedDur: 24 * time.Hour},
			uptime:  2 * time.Hour,
			wantDeg: []string{},
		},
		{
			name: "auth-failing boundary + up-guard + dead double-count",
			proxies: map[string]ProxyEntry{
				"up":      {ID: 1, Health: "up", Source: "file", AuthFailures: 1000},
				"off_hi":  {ID: 2, Health: "offline", Source: "file", AuthFailures: 200},
				"off_lo":  {ID: 3, Health: "offline", Source: "file", AuthFailures: 10},
				"dead_hi": {ID: 4, Health: "dead", Source: "file", AuthFailures: 500},
			},
			opts:     removeDeadOptions{authFailMin: 100},
			uptime:   48 * time.Hour, // days = 2 -> threshold = 100*2 = 200
			wantDead: []string{"dead_hi"},
			// degradedDur=0 (no --degraded) => offline entries are NOT degraded.
			wantDeg:  []string{},
			wantAuth: []string{"off_hi", "dead_hi"}, // dead_hi double-counts (dead AND auth-failing) - pinned
		},
		{
			name: "auth days-multiplier truncates at 47h59m (days=1) not 48h (days=2)",
			proxies: map[string]ProxyEntry{
				"e": {ID: 1, Health: "offline", Source: "file", AuthFailures: 150},
			},
			opts:     removeDeadOptions{authFailMin: 100},
			uptime:   47*time.Hour + 59*time.Minute, // days = 1 -> threshold 100 -> 150 >= 100 selected
			wantDeg:  []string{},
			wantAuth: []string{"e"},
		},
		{
			name: "auth days-multiplier at 48h (days=2) raises threshold above 150",
			proxies: map[string]ProxyEntry{
				"e": {ID: 1, Health: "offline", Source: "file", AuthFailures: 150},
			},
			opts:     removeDeadOptions{authFailMin: 100},
			uptime:   48 * time.Hour, // days = 2 -> threshold 200 -> 150 < 200 NOT selected
			wantDeg:  []string{},
			wantAuth: []string{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := &ProxyState{Source: c.stateSrc, StartedAt: time.Now().Add(-2 * time.Hour), Proxies: c.proxies}
			if c.uptime == 0 {
				c.uptime = 2 * time.Hour
			}
			dead, inactive, degraded, authFailing := collectRemoveDeadCandidates(state, c.opts, c.uptime)
			assertCandidatesEqual(t, dead, c.wantDead...)
			assertCandidatesEqual(t, inactive, c.wantIna...)
			assertCandidatesEqual(t, degraded, c.wantDeg...)
			assertCandidatesEqual(t, authFailing, c.wantAuth...)
		})
	}
}

// TestRemoveDeadCommandChainsToSources proves the seam between the selection
// logic and the actual removal: proxies selected by collectRemoveDeadCandidates
// are bucketed into the right source and physically removed from the real proxy
// file, while live ones survive.
func TestRemoveDeadCommandChainsToSources(t *testing.T) {
	home := withTempHome(t)
	proxyFilePath := filepath.Join(home, "proxy.txt")
	if err := os.WriteFile(proxyFilePath, []byte("1.1.1.1:1080:u:p\n2.2.2.2:1080:u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}

	state := &ProxyState{
		Source:    proxyFilePath,
		StartedAt: time.Now().Add(-80 * time.Minute), // settled past the 65-min gate
		Proxies: map[string]ProxyEntry{
			identityKey("1.1.1.1:1080", "u"): {Health: "dead", Source: "file"},
			identityKey("2.2.2.2:1080", "u"): {Health: "up", Source: "file"},
		},
	}

	dead, _, _, _ := collectRemoveDeadCandidates(state, removeDeadOptions{}, 80*time.Minute)
	assertCandidatesEqual(t, dead, identityKey("1.1.1.1:1080", "u"))

	if err := removeDeadProxies(state, removalBucketsBySource(state, dead)); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(proxyFilePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != "2.2.2.2:1080:u:p\n" {
		t.Fatalf("live proxy was not preserved / dead proxy not removed; got %q", got)
	}
}

func TestProxyRemoveDead_PreviewChangesNothing(t *testing.T) {
	home := withTempHome(t)
	proxyFilePath := filepath.Join(home, "proxy.txt")
	if err := os.WriteFile(proxyFilePath, []byte("1.1.1.1:1080:u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		Source:    proxyFilePath,
		StartedAt: time.Now().Add(-80 * time.Minute),
		Proxies: map[string]ProxyEntry{
			identityKey("1.1.1.1:1080", "u"): {ID: 7, Health: "dead", Source: "file", DownSince: time.Now().Add(-3 * time.Hour).Format(time.RFC3339)},
		},
	}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		proxyRemoveDead(docopt.Opts{"--preview": true})
	})
	if !strings.Contains(out, "=== PREVIEW (no changes will be made) ===") {
		t.Fatalf("preview header missing: %q", out)
	}
	if !strings.Contains(out, "  1 dead:") || !strings.Contains(out, "down_since=3h 0m") {
		t.Fatalf("dead candidate line missing: %q", out)
	}
	if !strings.Contains(out, "Would remove 1 proxies total.") {
		t.Fatalf("preview total missing: %q", out)
	}

	b, _ := os.ReadFile(proxyFilePath)
	if string(b) != "1.1.1.1:1080:u:p\n" {
		t.Fatalf("a preview must not remove anything, got %q", b)
	}
}

func TestProxyRemoveDead_YesRemovesFromFileAndReportsNothingToRemove(t *testing.T) {
	home := withTempHome(t)
	proxyFilePath := filepath.Join(home, "proxy.txt")
	if err := os.WriteFile(proxyFilePath, []byte("1.1.1.1:1080:u:p\n2.2.2.2:1080:u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		Source:    proxyFilePath,
		StartedAt: time.Now().Add(-80 * time.Minute),
		Proxies: map[string]ProxyEntry{
			identityKey("1.1.1.1:1080", "u"): {ID: 1, Health: "dead", Source: "file"},
			identityKey("2.2.2.2:1080", "u"): {ID: 2, Health: "up", Source: "file"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		proxyRemoveDead(docopt.Opts{"--yes": true})
	})
	if !strings.Contains(out, "Removed 1 proxies. Reload triggered.") {
		t.Fatalf("unexpected output: %q", out)
	}
	if b, _ := os.ReadFile(proxyFilePath); string(b) != "2.2.2.2:1080:u:p\n" {
		t.Fatalf("file after removal = %q", b)
	}

	// Second run: the state still lists the removed identity as dead, but the
	// command must say there is nothing left to collect once the state agrees.
	state, _ := readProxyState()
	state.Proxies = map[string]ProxyEntry{}
	if err := writeProxyState(state); err != nil {
		t.Fatal(err)
	}
	out = captureStdout(t, func() {
		proxyRemoveDead(docopt.Opts{"--yes": true})
	})
	if !strings.Contains(out, "Nothing to remove.") {
		t.Fatalf("unexpected output: %q", out)
	}
}

// ---- auth-path wiring (P1(b)) ----

func resetProxyAuthHistory() {
	globalProxyFailureHistory = &proxyFailureHistory{failures: map[string]int{}}
	globalProvenProxies = &provenProxySet{proven: map[string]bool{}}
}

// TestProxyURLGiveUpRetryDelay_DoublesAndCaps pins the v33 ladder: 15m base,
// doubling per give-up, capped at 24h, with up to 20% jitter.
func TestProxyURLGiveUpRetryDelay_DoublesAndCaps(t *testing.T) {
	for _, c := range []struct {
		giveUp int
		min    time.Duration
		max    time.Duration
	}{
		{1, proxyURLGiveUpRetryBase, proxyURLGiveUpRetryBase + proxyURLGiveUpRetryBase/5},
		{2, 30 * time.Minute, 36 * time.Minute},
		{3, time.Hour, 72 * time.Minute},
		{4, 2 * time.Hour, 144 * time.Minute},
		{5, 4 * time.Hour, 4*time.Hour + 4*time.Hour/5},
		{10, proxyURLGiveUpRetryCap, proxyURLGiveUpRetryCap + proxyURLGiveUpRetryCap/5},
	} {
		got := proxyURLGiveUpRetryDelay(c.giveUp)
		if got < c.min || got > c.max {
			t.Errorf("give-up %d: delay %s, want [%s, %s]", c.giveUp, got, c.min, c.max)
		}
	}
	// The 1-indexed floor: a zero or negative count behaves as the first give-up.
	if got := proxyURLGiveUpRetryDelay(0); got < proxyURLGiveUpRetryBase {
		t.Errorf("give-up 0: delay %s, want at least the base", got)
	}
}

// TestProxyAuthGiveUp_EvictsURLSourcedAfterFourCycles: a URL-sourced identity
// is evicted from the URL cache on its fourth give-up instead of being
// requeued again (v33 proxyURLGiveUpEvictAfterCycles).
func TestProxyAuthGiveUp_EvictsURLSourcedAfterFourCycles(t *testing.T) {
	withTempHome(t)
	resetProxyAuthHistory()
	defer resetProxyAuthHistory()

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{"7.7.7.7:1080": {}}}); err != nil {
		t.Fatal(err)
	}
	settings := &connect.ProxySettings{Network: "tcp", Address: "7.7.7.7:1080"}

	for i := 1; i <= 3; i++ {
		result := proxyAuthGiveUp(settings, true)
		if result.evicted {
			t.Fatalf("give-up %d must not evict yet", i)
		}
		if result.retryIn <= 0 {
			t.Fatalf("give-up %d must record a backoff", i)
		}
		if globalProxyFailureHistory.Eligible(settings.Key(), time.Now()) {
			t.Fatalf("give-up %d must leave the identity ineligible until the backoff elapses", i)
		}
	}

	result := proxyAuthGiveUp(settings, true)
	if !result.evicted || result.giveUp != proxyURLGiveUpEvictAfterCycles {
		t.Fatalf("give-up %d must evict, got %+v", proxyURLGiveUpEvictAfterCycles, result)
	}
	state, _ := readProxyURLState()
	if _, ok := state.Cache["7.7.7.7:1080"]; ok {
		t.Fatal("the evicted address must be gone from the URL cache")
	}
	if _, ok := state.Blacklist["7.7.7.7:1080"]; !ok {
		t.Fatal("the evicted address must be blacklisted so a fetch cannot re-add it")
	}
}

// TestProxyAuthGiveUp_CuratedProxyBacksOffWithoutEviction: a file/internal
// identity never leaves the operator's config, so it only backs off.
func TestProxyAuthGiveUp_CuratedProxyBacksOffWithoutEviction(t *testing.T) {
	withTempHome(t)
	resetProxyAuthHistory()
	defer resetProxyAuthHistory()

	settings := &connect.ProxySettings{Network: "tcp", Address: "1.2.3.4:1080"}
	for i := 0; i < proxyURLGiveUpEvictAfterCycles+2; i++ {
		result := proxyAuthGiveUp(settings, false)
		if result.evicted {
			t.Fatalf("curated give-up %d must never evict", i+1)
		}
	}
	if globalProxyFailureHistory.Eligible(settings.Key(), time.Now()) {
		t.Fatal("a curated give-up must still back the identity off")
	}
}

// TestNoteProviderAuthFailure_GivesUpAtTheUnprovenCeiling: an identity that has
// never completed an authentication gives up after unprovenMaxAuthFailures
// failures, and the give-up leaves a backoff record.
func TestNoteProviderAuthFailure_GivesUpAtTheUnprovenCeiling(t *testing.T) {
	withTempHome(t)
	resetProxyAuthHistory()
	defer resetProxyAuthHistory()

	settings := &connect.ProxySettings{Network: "tcp", Address: "5.5.5.5:1080"}
	key := settings.Key()
	if got := proxyAuthMaxFailures(key); got != unprovenMaxAuthFailures {
		t.Fatalf("unproven ceiling = %d, want %d", got, unprovenMaxAuthFailures)
	}

	for i := 0; i < unprovenMaxAuthFailures; i++ {
		noteProviderAuthFailure(settings, context.DeadlineExceeded)
	}
	if got := globalProxyFailureHistory.FailureCount(key); got != unprovenMaxAuthFailures {
		t.Fatalf("failure count = %d, want %d", got, unprovenMaxAuthFailures)
	}
	if got := globalProxyFailureHistory.GiveUpCount(key); got != 1 {
		t.Fatalf("give-up count = %d, want 1 once the ceiling is reached", got)
	}
	if globalProxyFailureHistory.Eligible(key, time.Now()) {
		t.Fatal("the give-up must leave the identity ineligible until the backoff elapses")
	}

	// A proven identity gets the longer leash.
	noteProviderAuthSuccess(settings)
	if got := proxyAuthMaxFailures(key); got != provenMaxAuthFailures {
		t.Fatalf("proven ceiling = %d, want %d", got, provenMaxAuthFailures)
	}
}

// TestNoteProviderAuthSuccess_ClearsHistory: a completed authentication clears
// the failure, give-up and slow-retry records for that identity.
func TestNoteProviderAuthSuccess_ClearsHistory(t *testing.T) {
	withTempHome(t)
	resetProxyAuthHistory()
	defer resetProxyAuthHistory()

	settings := &connect.ProxySettings{Network: "tcp", Address: "6.6.6.6:1080"}
	key := settings.Key()
	globalProxyFailureHistory.RecordFailure(key)
	globalProxyFailureHistory.RecordGiveUp(key)
	globalProxyFailureHistory.SetBackoffUntil(key, time.Now().Add(time.Hour))
	globalProxySlowRetryState.RecordSlowRetryStart(key)

	noteProviderAuthSuccess(settings)

	if got := globalProxyFailureHistory.FailureCount(key); got != 0 {
		t.Errorf("failure count = %d after success", got)
	}
	if got := globalProxyFailureHistory.GiveUpCount(key); got != 0 {
		t.Errorf("give-up count = %d after success", got)
	}
	if !globalProxyFailureHistory.Eligible(key, time.Now()) {
		t.Error("success must clear the backoff window")
	}
	if !globalProvenProxies.HasSucceeded(key) {
		t.Error("success must mark the identity proven")
	}
	if globalProxySlowRetryState.WasDropped(key) {
		t.Error("success must clear a stale drop record")
	}
}

// TestNoteProviderAuthFailure_CancelledIsNotAFailure: a cancelled attempt says
// nothing about the proxy (v33 never counts context cancellation as a failure),
// while a timeout is counted: the miner's single-call auth path has no measured
// slow-versus-broken signal, which is the one documented deviation from v33's
// ladder.
func TestNoteProviderAuthFailure_CancelledIsNotAFailure(t *testing.T) {
	withTempHome(t)
	resetProxyAuthHistory()
	defer resetProxyAuthHistory()

	settings := &connect.ProxySettings{Network: "tcp", Address: "8.8.8.8:1080"}
	noteProviderAuthFailure(settings, context.Canceled)
	if got := globalProxyFailureHistory.FailureCount(settings.Key()); got != 0 {
		t.Fatalf("cancellation recorded %d failures, want 0", got)
	}
	noteProviderAuthFailure(settings, context.DeadlineExceeded)
	if got := globalProxyFailureHistory.FailureCount(settings.Key()); got != 1 {
		t.Fatalf("a timeout must be counted (recorded %d failures, want 1)", got)
	}
}

// TestNoteProviderAuthFailure_DirectSlotIsIgnored: the native direct slot has
// no proxy identity to account for.
func TestNoteProviderAuthFailure_DirectSlotIsIgnored(t *testing.T) {
	resetProxyAuthHistory()
	defer resetProxyAuthHistory()

	noteProviderAuthFailure(nil, context.DeadlineExceeded)
	noteProviderAuthSuccess(nil)
	if keys := globalProxyFailureHistory.Keys(); len(keys) != 0 {
		t.Fatalf("direct slot recorded history: %v", keys)
	}
}

// TestDropBackoffHeldURLProxies_SkipsWindow is the launch-time enforcement
// test: a URL-sourced identity whose give-up backoff window has not elapsed is
// held out of the desired set, while an eligible one is kept. Before this gate
// any rebuild of the desired set relaunched a dead address immediately and the
// escalating backoff never took effect.
func TestDropBackoffHeldURLProxies_SkipsWindow(t *testing.T) {
	resetProxyAuthHistory()
	defer resetProxyAuthHistory()

	midBackoff := &connect.ProxySettings{Network: "tcp", Address: "7.7.7.7:1080"}
	eligible := &connect.ProxySettings{Network: "tcp", Address: "8.8.8.8:1080"}
	curated := &connect.ProxySettings{Network: "tcp", Address: "9.9.9.9:1080"}

	globalProxyFailureHistory.SetBackoffUntil(midBackoff.Key(), time.Now().Add(time.Hour))
	globalProxyFailureHistory.SetBackoffUntil(curated.Key(), time.Now().Add(time.Hour))

	desiredSet := map[string]*connect.ProxySettings{
		midBackoff.Key(): midBackoff,
		eligible.Key():   eligible,
		curated.Key():    curated,
	}
	sourceOf := map[string]string{
		midBackoff.Key(): "url",
		eligible.Key():   "url",
		curated.Key():    "file",
	}

	if held := dropBackoffHeldURLProxies(desiredSet, sourceOf, time.Now()); held != 1 {
		t.Fatalf("held = %d, want 1", held)
	}
	if _, ok := desiredSet[midBackoff.Key()]; ok {
		t.Error("a URL proxy still inside its backoff window must be held back")
	}
	if _, ok := desiredSet[eligible.Key()]; !ok {
		t.Error("an eligible URL proxy must stay in the desired set")
	}
	if _, ok := desiredSet[curated.Key()]; !ok {
		t.Error("a curated proxy must never be held by the URL backoff gate")
	}
	if _, ok := sourceOf[midBackoff.Key()]; ok {
		t.Error("a held identity must leave the provenance map too")
	}
}

// TestProxyManagerDesired_HoldsBackoffURLProxy drives the same gate through the
// manager's desired-set builder: a URL-cached address inside its backoff window
// is not part of the launch set, and rejoins it once the window elapses.
func TestProxyManagerDesired_HoldsBackoffURLProxy(t *testing.T) {
	withTempHome(t)
	resetProxyAuthHistory()
	defer resetProxyAuthHistory()

	if err := writeProxyURLState(&ProxyURLState{Cache: map[string]ProxyURLEntry{
		"7.7.7.7:1080": {},
		"8.8.8.8:1080": {},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}

	globalProxyFailureHistory.SetBackoffUntil("7.7.7.7:1080", time.Now().Add(time.Hour))

	m := newProxyManager(defaultProxyManagerConfig())
	launch, _, _, err := m.desired(false)
	if err != nil {
		t.Fatal(err)
	}
	var addresses []string
	for _, s := range launch {
		addresses = append(addresses, s.Address)
	}
	sort.Strings(addresses)
	if !reflect.DeepEqual(addresses, []string{"8.8.8.8:1080"}) {
		t.Fatalf("launch = %v, want only the eligible URL proxy", addresses)
	}

	// Once the window elapses the held address is desired again.
	globalProxyFailureHistory.Reset("7.7.7.7:1080")
	launch, _, _, err = m.desired(false)
	if err != nil {
		t.Fatal(err)
	}
	addresses = nil
	for _, s := range launch {
		addresses = append(addresses, s.Address)
	}
	sort.Strings(addresses)
	if !reflect.DeepEqual(addresses, []string{"7.7.7.7:1080", "8.8.8.8:1080"}) {
		t.Fatalf("launch after backoff = %v, want both URL proxies", addresses)
	}
}

// TestProxyProvideRecordsAuthOutcomeInHistory is the wiring proof for the miner
// auth path: the provide loop's failure hook is what feeds the ported
// accounting, so an identity that fails accumulates history that the desired
// set then honors.
func TestProxyProvideRecordsAuthOutcomeInHistory(t *testing.T) {
	withTempHome(t)
	resetProxyAuthHistory()
	defer resetProxyAuthHistory()

	if err := writeProxyURLState(&ProxyURLState{
		Cache: map[string]ProxyURLEntry{"4.4.4.4:1080": {}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{
		Proxies: map[string]ProxyEntry{"4.4.4.4:1080": {ID: 1, Health: "up", Source: "url"}},
	}); err != nil {
		t.Fatal(err)
	}

	settings := &connect.ProxySettings{Network: "tcp", Address: "4.4.4.4:1080"}
	for i := 0; i < unprovenMaxAuthFailures; i++ {
		noteProviderAuthFailure(settings, context.DeadlineExceeded)
	}
	if !isURLSourcedProxyKey(settings.Key()) {
		t.Fatal("the failure hook must classify a state entry tagged source=url as URL-sourced")
	}
	if globalProxyFailureHistory.Eligible(settings.Key(), time.Now()) {
		t.Fatal("failure to the ceiling must leave the identity backed off")
	}

	m := newProxyManager(defaultProxyManagerConfig())
	launch, _, _, err := m.desired(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(launch) != 0 {
		t.Fatalf("a backed-off URL proxy must not be relaunched, got %v", launch)
	}
}
