package miner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
)

// Both list forms must parse to the same address and credentials, including
// credential characters that look like the separators themselves.
func TestParseProxyEntryCredentialForms(t *testing.T) {
	auths := map[string]*ProxyAuth{"home": {User: "u", Password: "p"}}
	tests := []struct {
		name     string
		entry    string
		key      string
		address  string
		user     string
		password string
	}{
		{"address only", "10.0.0.1:1080", "", "10.0.0.1:1080", "", ""},
		{"colon form", "10.0.0.1:1080:bob:secret", "", "10.0.0.1:1080", "bob", "secret"},
		{"colon form bracketed ipv6", "[2001:db8::1]:1080:bob:secret", "", "[2001:db8::1]:1080", "bob", "secret"},
		{"colon form colon password", "10.0.0.1:1080:bob:se:cret", "", "10.0.0.1:1080", "bob", "se:cret"},
		{"colon form at password", "10.0.0.1:1080:bob:se@cret", "", "10.0.0.1:1080", "bob", "se@cret"},
		{"userinfo form", "bob:secret@10.0.0.1:1080", "", "10.0.0.1:1080", "bob", "secret"},
		{"userinfo colon password", "bob:se:cret@10.0.0.1:1080", "", "10.0.0.1:1080", "bob", "se:cret"},
		{"userinfo at password", "bob:se@cret@10.0.0.1:1080", "", "10.0.0.1:1080", "bob", "se@cret"},
		{"registered key", "home@10.0.0.1:1080", "home", "10.0.0.1:1080", "", ""},
		{"unregistered key", "spare@10.0.0.1:1080", "spare", "10.0.0.1:1080", "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entry := parseProxyEntry(test.entry, auths)
			if entry.key != test.key ||
				entry.address != test.address ||
				entry.user != test.user ||
				entry.password != test.password {
				t.Fatalf(
					"parseProxyEntry(%q) = key %q address %q user %q password %q, want key %q address %q user %q password %q",
					test.entry,
					entry.key,
					entry.address,
					entry.user,
					entry.password,
					test.key,
					test.address,
					test.user,
					test.password,
				)
			}
		})
	}
}

// A list may mix the two forms, and remove must match an entry written in the
// other form by reaching the one canonical key.
func TestProxyAddRemoveRoundTripCredentialForms(t *testing.T) {
	state := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", state)

	entries := []string{
		"10.0.0.1:1080:bob:se:cret",
		"10.0.0.2:1081:alice:p@ss",
		"carol:pa:ss@10.0.0.3:1082",
	}
	listPath := filepath.Join(state, "proxies.txt")
	if err := os.WriteFile(listPath, []byte(strings.Join(entries, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	proxyAdd(docopt.Opts{"<key_address>": []string{}, "--proxy_file": listPath, "-f": true})

	want := map[string][2]string{
		"10.0.0.1:1080": {"bob", "se:cret"},
		"10.0.0.2:1081": {"alice", "p@ss"},
		"10.0.0.3:1082": {"carol", "pa:ss"},
	}
	settings := readProxySettings()
	if len(settings) != len(want) {
		t.Fatalf("read %d proxies, want %d", len(settings), len(want))
	}
	for _, proxySettings := range settings {
		expected, ok := want[proxySettings.Address]
		if !ok {
			t.Fatalf("unexpected address %q", proxySettings.Address)
		}
		if proxySettings.Auth == nil ||
			proxySettings.Auth.User != expected[0] ||
			proxySettings.Auth.Password != expected[1] {
			t.Fatalf("address %q lost its credentials", proxySettings.Address)
		}
	}

	// Remove the colon-form entry by its userinfo spelling.
	proxyRemove(docopt.Opts{
		"<key_address>": []string{"bob:se:cret@10.0.0.1:1080"},
		"--all":         false,
	})
	if remaining := readProxySettings(); len(remaining) != 2 {
		t.Fatalf("remove by the other form left %d proxies, want 2", len(remaining))
	}

	// Remove the userinfo-form entry by its colon spelling.
	proxyRemove(docopt.Opts{
		"<key_address>": []string{"10.0.0.3:1082:carol:pa:ss"},
		"--all":         false,
	})
	remaining := readProxySettings()
	if len(remaining) != 1 {
		t.Fatalf("remove by the other form left %d proxies, want 1", len(remaining))
	}
	if remaining[0].Address != "10.0.0.2:1081" ||
		remaining[0].Auth == nil ||
		remaining[0].Auth.User != "alice" ||
		remaining[0].Auth.Password != "p@ss" {
		t.Fatal("the wrong proxy survived removal")
	}

	// Remove remaining entry by address alone
	proxyRemove(docopt.Opts{
		"<key_address>": []string{"10.0.0.2:1081"},
		"--all":         false,
	})
	if remaining := readProxySettings(); len(remaining) != 0 {
		t.Fatalf("remove by address alone left %d proxies, want 0", len(remaining))
	}
}

// Proxies added with registered auth keys must keep their credentials outside
// map keys, support credential rotation, and remove cleanly via key@address
// or address alone.
func TestProxyAddRemoveRegisteredAuthKeys(t *testing.T) {
	state := t.TempDir()
	t.Setenv("URNETWORK_STATE_DIR", state)

	opts, err := docopt.ParseArgs(mainUsage(), []string{"proxy", "auth", "add", "home", "alice", "supersecret", "-f"}, "")
	if err != nil {
		t.Fatalf("docopt parse error: %v", err)
	}
	proxyAuthAdd(opts)

	proxyAdd(docopt.Opts{"<key_address>": []string{"home@10.0.0.1:1080"}, "-f": true})
	proxyAdd(docopt.Opts{"<key_address>": []string{"home@10.0.0.2:1080"}, "-f": true})

	cfg := readProxyConfig()
	// Plain address must be the map key, never the secret password
	if cfg.Servers["10.0.0.1:1080"] != "home" {
		t.Fatalf("expected map key 10.0.0.1:1080 -> home, got servers: %+v", cfg.Servers)
	}
	for k := range cfg.Servers {
		if strings.Contains(k, "supersecret") {
			t.Fatalf("credentials leaked into dictionary key: %q", k)
		}
	}

	settings := readProxySettings()
	if len(settings) != 2 {
		t.Fatalf("expected 2 proxies, got %d", len(settings))
	}
	for _, s := range settings {
		if s.Auth == nil || s.Auth.User != "alice" || s.Auth.Password != "supersecret" {
			t.Fatalf("proxy %s did not resolve auth: %+v", s.Address, s.Auth)
		}
	}

	// Credential rotation: updating auth password dynamically updates readProxySettings
	optsRotated, err := docopt.ParseArgs(mainUsage(), []string{"proxy", "auth", "add", "home", "alice", "rotatedpassword", "-f"}, "")
	if err != nil {
		t.Fatalf("docopt parse error: %v", err)
	}
	proxyAuthAdd(optsRotated)
	rotated := readProxySettings()
	for _, s := range rotated {
		if s.Auth == nil || s.Auth.Password != "rotatedpassword" {
			t.Fatalf("proxy %s did not reflect rotated auth password: %+v", s.Address, s.Auth)
		}
	}

	// Remove first proxy via home@10.0.0.1:1080
	proxyRemove(docopt.Opts{
		"<key_address>": []string{"home@10.0.0.1:1080"},
		"--all":         false,
	})
	if remaining := readProxySettings(); len(remaining) != 1 || remaining[0].Address != "10.0.0.2:1080" {
		t.Fatalf("remove by key@address left %+v, expected only 10.0.0.2:1080", remaining)
	}

	// Remove second proxy via 10.0.0.2:1080 (address alone)
	proxyRemove(docopt.Opts{
		"<key_address>": []string{"10.0.0.2:1080"},
		"--all":         false,
	})
	if remaining := readProxySettings(); len(remaining) != 0 {
		t.Fatalf("remove by address alone left %+v, expected 0", remaining)
	}
}

// The stored form itself is read back by the same rules, so a colon in a
// password cannot be mistaken for another field.
func TestParseProxyAddressStoredForm(t *testing.T) {
	address, user, password := parseProxyAddress("10.0.0.1:1080:bob:se:cret")
	if address != "10.0.0.1:1080" || user != "bob" || password != "se:cret" {
		t.Fatalf("stored form parsed as %q %q %q", address, user, password)
	}
}
