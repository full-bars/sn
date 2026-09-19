//go:build linux

package urnettools

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A host unit whose NAME merely contains a runtime word is not a container;
// real runtime scopes/slices are.
func TestClassifyContainerCgroupNearMisses(t *testing.T) {
	cases := []struct {
		cg   string
		want bool
	}{
		{"0::/system.slice/urnetwork-docker-helper.service", false},
		{"0::/system.slice/lxcfs.service", false},
		{"0::/system.slice/containerd.service", false},
		{"0::/system.slice/docker.service", false},
		{"0::/system.slice/my-libpod-thing.service", false},
		{"0::/system.slice/kubepods-monitor.service", false},
		{"0::/system.slice/docker-abc123.scope", true},
		{"5:cpu:/docker/abc123456789", true},
		{"0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podx.slice/cri-containerd-abc.scope", true},
		{"0::/kubepods/burstable/pod123/abc123", true},
		{"0::/user.slice/user-1000.slice/user@1000.service/user.slice/libpod-abc123.scope", true},
		{"7:devices:/lxc.payload.abc123/container", true},
		{"0::/machine.slice/machine-foo.scope", true},
		{"12:pids:/user.slice\n0::/system.slice/docker-abc.scope\n", true},
	}
	for _, c := range cases {
		if got := classifyContainerByNamespaceAndCgroup(true, c.cg); got != c.want {
			t.Errorf("classify(%q) = %v, want %v", c.cg, got, c.want)
		}
	}
}

// Discovery must not trust a process-controlled HOME as a state directory.
func TestResolveDiscoveredStateDir(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	def := filepath.Join(home, ".urnetwork")
	cases := []struct {
		name, ownerHome, envDir, want string
	}{
		{"unresolved owner never attributes env HOME", "", "/root/.urnetwork", ""},
		{"unresolved owner, no env", "", "", ""},
		{"no env uses owner home", home, "", def},
		{"legit subdirectory honored", home, filepath.Join(home, "sub", ".urnetwork"), filepath.Join(home, "sub", ".urnetwork")},
		{"outside the home rejected", home, filepath.Join(outside, ".urnetwork"), def},
		{"lexically inside but symlinked outside", home, filepath.Join(home, "link", ".urnetwork"), def},
		{"root HOME rejected", home, "/root/.urnetwork", def},
		{"owner home itself rejected", home, home, def},
	}
	for _, c := range cases {
		if got := resolveDiscoveredStateDir(c.ownerHome, c.envDir); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// The --state-dir argv override goes through the same containment check, and
// an owner with no resolvable home has nothing to validate against.
func TestStateDirInsideHome(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, "link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	for _, c := range []struct {
		name, home, dir string
		want            bool
	}{
		{"inside, not yet created", home, filepath.Join(home, "new", ".urnetwork"), true},
		{"the home itself", home, home, false},
		{"sibling with a shared prefix", home, home + "-evil/.urnetwork", false},
		{"symlink escaping the home", home, filepath.Join(home, "link"), false},
		{"path under an escaping symlink", home, filepath.Join(home, "link", "x"), false},
		{"empty home", "", "/root/.urnetwork", false},
		{"empty dir", home, "", false},
	} {
		if got := stateDirInsideHome(c.home, c.dir); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestStateDirArgvOverrideValidation pins the --state-dir argv validation
// block of discoverProcesses: the override is accepted only when it provably
// stays inside the KERNEL-reported owner home (ownerHome captured from the
// single processOwner call), and an owner whose home cannot be resolved has
// nothing to validate against, so the argv value is rejected, not believed.
// The framing mirrors the production block exactly: accept = the override is
// non-empty and stateDirInsideHome(ownerHome, override).
func TestStateDirArgvOverrideValidation(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	cases := []struct {
		name, ownerHome, argvDir string
		// wantAccept is whether the argv override survives validation.
		wantAccept bool
	}{
		{"inside the owner home accepted", home, filepath.Join(home, "sub", ".urnetwork"), true},
		{"stale ownerHome reuse: argv outside the resolved home rejected", home, filepath.Join(outside, ".urnetwork"), false},
		{"owner home itself rejected", home, home, false},
		{"unresolvable owner home has no trusted root to validate against", "", filepath.Join(home, "sub", ".urnetwork"), false},
		{"empty override rejected", home, "", false},
		{"lexically inside but symlinked outside rejected", home, filepath.Join(home, "link", ".urnetwork"), false},
	}
	for _, c := range cases {
		got := c.argvDir != "" && stateDirInsideHome(c.ownerHome, c.argvDir)
		if got != c.wantAccept {
			t.Errorf("%s: accepted=%v, want %v (ownerHome=%q, argvDir=%q)", c.name, got, c.wantAccept, c.ownerHome, c.argvDir)
		}
		// The rejected override must never be attributed: resolution falls
		// back to <ownerHome>/.urnetwork (or nothing without a home).
		gotDir := resolveDiscoveredStateDir(c.ownerHome, c.argvDir)
		wantDir := c.argvDir
		if !c.wantAccept {
			wantDir = filepath.Join(c.ownerHome, ".urnetwork")
			if c.ownerHome == "" {
				wantDir = ""
			}
		}
		if gotDir != wantDir {
			t.Errorf("%s: resolveDiscoveredStateDir = %q, want %q", c.name, gotDir, wantDir)
		}
	}
}

// A longer replacement version must actually be what the scanner resolves:
// the original stamp is invalidated before the new one is appended.
func TestRewriteVersionStampLongerVersionWins(t *testing.T) {
	payload := []byte("\x7fELFjunk\n" + stampOf("1.2.3") + "\x00tail")
	longer := "v" + strings.Repeat("9", 5) + ".0.0-fix.31.0"
	got := scanVersionStamp(bytes.NewReader(rewriteVersionStamp(payload, longer)))
	if got != longer {
		t.Fatalf("scanner resolved %q, want the longer replacement %q", got, longer)
	}
	shorter := "v1.2"
	if got := scanVersionStamp(bytes.NewReader(rewriteVersionStamp(payload, shorter))); got != shorter {
		t.Fatalf("shorter replacement: got %q, want %q", got, shorter)
	}
}
