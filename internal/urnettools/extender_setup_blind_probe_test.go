package urnettools

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// `ss -p` hides root-owned sockets from an unprivileged caller, so a held port
// with no visible holder must not abort setup before it can elevate.
func TestExtenderSetupHiddenPortHolderDefersToElevation(t *testing.T) {
	ssOut := "State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\nLISTEN 0 4096 *:443 *:*\n"
	run := func(root bool) (struct{}, error) {
		cfg := &mockPlatformConfigurer{platform: "linux"}
		err := RunExtenderSetup(ExtenderSetupOptions{
			Platform: "linux", PlatformCfg: cfg, IsRoot: func() bool { return root },
			Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Force: true,
			LookPath: func(string) (string, error) { return "/usr/bin/sudo", nil },
			PortChecker: &PortChecker{Platform: "linux", ExecCmd: func(name string, args ...string) ([]byte, error) {
				if args[0] == "-tlnp" {
					return []byte(ssOut), nil
				}
				return nil, nil
			}},
			FirewallMgr: &FirewallManager{Platform: "linux", LookPath: func(string) (string, error) { return "", errors.New("missing") }},
		})
		return struct{}{}, err
	}
	// Non-root sees nothing about the holder: must reach elevation.
	// Without a TTY it stops at the "run sudo" instruction, which is the
	// elevation step; the failure it must not produce is a port conflict.
	if _, err := run(false); err != nil && (strings.Contains(err.Error(), "port TCP 443 is held") || !strings.Contains(err.Error(), "requires root privileges")) {
		t.Fatalf("hidden holder aborted setup before elevation: %v", err)
	}
	// Root sees everything: an unidentified holder is still a conflict.
	if _, err := run(true); err == nil || !strings.Contains(err.Error(), "port TCP 443 is held") {
		t.Fatalf("root run must still report the conflict: %v", err)
	}
}

func TestExtenderFirewallUFWUnreadableIsNotNoFirewall(t *testing.T) {
	fm := &FirewallManager{Platform: "linux",
		LookPath: func(name string) (string, error) {
			if name == "ufw" {
				return name, nil
			}
			return "", errors.New("missing")
		},
		ExecCmd: func(string, ...string) ([]byte, error) {
			return []byte("ERROR: You need to be root to run this script"), errors.New("exit 1")
		}}
	st := fm.Detect()
	if st.Kind != FirewallUFW || !st.Active || st.PortsOpen {
		t.Fatalf("unreadable ufw misclassified: %+v", st)
	}
}
