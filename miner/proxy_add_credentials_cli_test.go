package miner

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxyAddCredentialsCLIChild(t *testing.T) {
	if os.Getenv("SN_PROXY_ADD_CHILD") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("SN_PROXY_ADD_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	Run(args)
}

func TestProxyAddCredentialsCLI(t *testing.T) {
	for _, fileBacked := range []bool{false, true} {
		for _, tc := range []struct {
			name, input, written string
			fail                 bool
		}{
			{"bare", "127.0.0.1:1080", "127.0.0.1:1080", false},
			{"userinfo", "alice:syntheticpass@127.0.0.1:1080", "alice:syntheticpass@127.0.0.1:1080", false},
			{"malformed", "alice:syntheticpass@proxy:bad", "", true},
		} {
			mode := "internal"
			if fileBacked {
				mode = "file"
			}
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				dir := t.TempDir()
				source := filepath.Join(dir, "source.txt")
				if fileBacked {
					if err := os.WriteFile(source, nil, 0600); err != nil {
						t.Fatal(err)
					}
					state, err := json.Marshal(ProxyState{Source: source, Proxies: map[string]ProxyEntry{}})
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "proxy.state"), state, 0600); err != nil {
						t.Fatal(err)
					}
				}
				args, _ := json.Marshal([]string{"proxy", "add", tc.input, "-f"})
				cmd := exec.Command(os.Args[0], "-test.run=^TestProxyAddCredentialsCLIChild$")
				cmd.Env = append(os.Environ(), "SN_PROXY_ADD_CHILD=1", "SN_PROXY_ADD_ARGS="+string(args), "HOME="+dir, "URNETWORK_STATE_DIR="+dir, "WARP_VERSION=synthetic")
				out, err := cmd.CombinedOutput()
				if (err != nil) != tc.fail {
					t.Fatalf("exit error=%v output=%s", err, out)
				}
				if strings.Contains(string(out), "syntheticpass") {
					t.Fatalf("password in output: %s", out)
				}
				if tc.fail && !strings.Contains(string(out), "invalid proxy") {
					t.Fatalf("missing diagnostic: %s", out)
				}
				path := source
				if !fileBacked {
					path = filepath.Join(dir, "proxy")
				}
				data, readErr := os.ReadFile(path)
				if tc.fail {
					if readErr == nil && len(data) != 0 {
						t.Fatalf("invalid input wrote %q", data)
					}
					if readErr != nil && !os.IsNotExist(readErr) {
						t.Fatal(readErr)
					}
				} else {
					if readErr != nil {
						t.Fatal(readErr)
					}
					expected := tc.written
					if !fileBacked && tc.name == "userinfo" {
						expected = "127.0.0.1:1080:alice:syntheticpass"
					}
					if !strings.Contains(string(data), expected) {
						t.Fatalf("written %q, want %q", data, expected)
					}
					if !strings.Contains(string(out), "added server") {
						t.Fatalf("missing success: %s", out)
					}
				}
			})
		}
	}
}

func TestProxyAddFileBackedKeyAmbiguity(t *testing.T) {
	withTempHome(t)
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, nil, 0600); err != nil {
		t.Fatal(err)
	}
	writeProxyConfig(&ProxyConfig{Auths: map[string]*ProxyAuth{"alice:syntheticpass": {User: "bob", Password: "secret"}}, Servers: map[string]string{}})
	err := proxyAddFileBacked(source, parseArgsForTest(t, []string{"proxy", "add", "alice:syntheticpass@127.0.0.1:1080", "-f"}))
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || strings.Contains(err.Error(), "syntheticpass") {
		t.Fatalf("ambiguous entry: %v", err)
	}
	data, err := os.ReadFile(source)
	if err != nil || len(data) != 0 {
		t.Fatalf("source changed: %q %v", data, err)
	}
}

func TestProxyAddFileBackedRegisteredKey(t *testing.T) {
	withTempHome(t)
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, nil, 0600); err != nil {
		t.Fatal(err)
	}
	writeProxyConfig(&ProxyConfig{Auths: map[string]*ProxyAuth{"home": {User: "alice", Password: "syntheticpass"}}, Servers: map[string]string{}})
	err := proxyAddFileBacked(source, parseArgsForTest(t, []string{"proxy", "add", "home@127.0.0.1:1080", "-f"}))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "127.0.0.1:1080:alice:syntheticpass\n" {
		t.Fatalf("credentials missing: %q", data)
	}
}
