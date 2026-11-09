package urnettools

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestFirewalldRuntimeAndPermanentWithoutReload(t *testing.T) {
	var writes [][]string
	fm := &FirewallManager{Platform: "linux", LookPath: func(s string) (string, error) {
		if s == "firewall-cmd" {
			return s, nil
		}
		return "", errors.New("missing")
	}, ExecCmd: func(_ string, args ...string) ([]byte, error) {
		if args[0] == "--state" {
			return []byte("running"), nil
		}
		if args[0] == "--get-default-zone" {
			return []byte("public"), nil
		}
		for _, a := range args {
			if strings.HasPrefix(a, "--query-port=") {
				if a == "--query-port=443/tcp" && args[0] != "--permanent" {
					return []byte("yes"), nil
				}
				return []byte("no"), errors.New("not enabled")
			}
		}
		writes = append(writes, append([]string(nil), args...))
		return nil, nil
	}}
	st := fm.Detect()
	want := [][]string{{"--permanent", "--zone=public", "--add-port=443/tcp"}, {"--zone=public", "--add-port=443/udp"}, {"--permanent", "--zone=public", "--add-port=443/udp"}, {"--zone=public", "--add-port=4053/udp"}, {"--permanent", "--zone=public", "--add-port=4053/udp"}}
	if st.PortsOpen {
		t.Fatal("permanent rule missing")
	}
	ok, err := fm.Apply(st)
	if !ok || err != nil || !reflect.DeepEqual(writes, want) {
		t.Fatalf("applied=%v err=%v writes=%v", ok, err, writes)
	}
}

func TestFirewalldUnknownFailsClosed(t *testing.T) {
	writes := 0
	fm := &FirewallManager{Platform: "linux", LookPath: func(s string) (string, error) {
		if s == "firewall-cmd" {
			return s, nil
		}
		return "", errors.New("missing")
	}, ExecCmd: func(_ string, args ...string) ([]byte, error) {
		if args[0] == "--state" {
			return []byte("running"), nil
		}
		if args[0] == "--get-default-zone" {
			return nil, errors.New("denied")
		}
		writes++
		return nil, nil
	}}
	st := fm.Detect()
	ok, err := fm.Apply(st)
	if st.Kind != FirewallFirewalld || ok || err == nil || writes != 0 {
		t.Fatalf("status=%+v applied=%v err=%v writes=%d", st, ok, err, writes)
	}
	ok, err = fm.Apply(FirewallStatus{Kind: FirewallFirewalld})
	if ok || err == nil || writes != 0 {
		t.Fatalf("empty plan applied=%v err=%v writes=%d", ok, err, writes)
	}
}

func TestExtenderSetupUnverifiedFirewalldFailsBeforeMutation(t *testing.T) {
	oldDiscover := setupDiscoverFn
	setupDiscoverFn = func() []Provider {
		return []Provider{{Binary: "/usr/local/bin/urnetwork", User: "provider"}}
	}
	defer func() { setupDiscoverFn = oldDiscover }()

	cfg := &mockPlatformConfigurer{
		platform:   "linux",
		configured: false,
	}

	fwWrites := 0
	fm := &FirewallManager{
		Platform: "linux",
		LookPath: func(s string) (string, error) {
			if s == "firewall-cmd" {
				return s, nil
			}
			return "", errors.New("missing")
		},
		ExecCmd: func(_ string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "--state" {
				return []byte("running"), nil
			}
			if len(args) > 0 && args[0] == "--get-default-zone" {
				return nil, errors.New("zone permission denied")
			}
			fwWrites++
			return nil, nil
		},
	}

	var out bytes.Buffer
	err := RunExtenderSetup(ExtenderSetupOptions{
		Route:       "capability",
		Platform:    "linux",
		PlatformCfg: cfg,
		FirewallMgr: fm,
		PortChecker: &PortChecker{Platform: "linux", ExecCmd: func(string, ...string) ([]byte, error) { return nil, nil }},
		IsRoot:      func() bool { return true },
		Yes:         true,
		Stdout:      &out,
		Stderr:      &out,
	})

	if err == nil {
		t.Fatal("expected error on unverified firewalld, got nil")
	}
	if !strings.Contains(err.Error(), "firewalld is active but its rules could not be inspected") || !strings.Contains(err.Error(), "zone permission denied") {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.applyCalled {
		t.Fatal("platform action applied before firewalld verification failed")
	}
	if fwWrites != 0 {
		t.Fatalf("firewall was mutated %d times", fwWrites)
	}
	if strings.Contains(out.String(), "--permanent") || strings.Contains(out.String(), "--reload") {
		t.Fatalf("unverified commands were previewed: %q", out.String())
	}
}
