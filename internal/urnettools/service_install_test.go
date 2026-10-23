package urnettools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// renderOpenRCServiceScript is the Go side of the OpenRC init script that
// scripts/Provider_Install_Linux.sh writes. The shell installer cannot call
// into Go, so the two templates are kept in step by hand; the shape test below
// asserts the directives that make the service work, and
// TestOpenRCRenderMatchesInstallerDriftMarkers asserts the static directives
// the installer also carries.
func TestRenderOpenRCServiceScriptShape(t *testing.T) {
	script := renderOpenRCServiceScript(
		"urnetwork",
		"/usr/local/lib/urnetwork-provider/bin/urnetwork",
		"urnet",
		"/home/urnet",
	)

	for _, want := range []struct{ name, needle string }{
		// An openrc-run script without this shebang is not an OpenRC script
		// at all; rc-service would try to run it as plain sh.
		{"openrc-run shebang", "#!/sbin/openrc-run"},
		{"provider command", `command="/usr/local/lib/urnetwork-provider/bin/urnetwork"`},
		{"provide subcommand", `command_args="provide"`},
		{"service user", `command_user="urnet"`},
		// Without supervisor= the provider runs as a foreground child of
		// rc-service: start never returns and nothing respawns a crash.
		{"supervisor", `supervisor="supervise-daemon"`},
		// supervise-daemon sets neither INVOCATION_ID nor NOTIFY_SOCKET. Without
		// this marker the provider refuses the watchdog's supervised exit 75.
		{"init marker", "export URNETWORK_INIT=openrc"},
		// A hung provider must be killed, or rc-service stop reports success
		// while the old process keeps the identity and the state dir.
		{"stop schedule", `retry="TERM/40/KILL/5"`},
		// Unlimited respawn turns a binary that cannot start into a 5 second
		// crash loop; these bound it the way the systemd unit is bounded.
		{"respawn delay", "respawn_delay=5"},
		{"respawn max", "respawn_max=10"},
		{"respawn period", "respawn_period=3600"},
		{"stdout log", `output_log="/var/log/urnetwork.log"`},
		{"stderr log", `error_log="/var/log/urnetwork.err"`},
		// supervise-daemon --stop busy-loops when the pidfile is empty, so a
		// stop of a never-started service must be short-circuited.
		{"stop guard pidfile", "/var/run/supervise-urnetwork.pid"},
		{"stop guard", "default_stop"},
		{"net dependency", "use net"},
		{"firewall ordering", "after firewall"},
	} {
		if !strings.Contains(script, want.needle) {
			t.Errorf("OpenRC script lacks the %s (%q)", want.name, want.needle)
		}
	}
}

// A beta build and a stable build on one box must not share a log file or a
// supervise pidfile, for the same reason bsdRcServiceName derives the service
// name from the binary: two providers writing one file makes both unreadable.
func TestRenderOpenRCServiceScriptNamesLogsAfterService(t *testing.T) {
	script := renderOpenRCServiceScript(
		"urnetwork_beta",
		"/usr/local/lib/urnetwork-provider/beta/urnetwork",
		"urnet",
		"/home/urnet",
	)
	for _, want := range []string{
		`output_log="/var/log/urnetwork_beta.log"`,
		`error_log="/var/log/urnetwork_beta.err"`,
		"/var/run/supervise-urnetwork_beta.pid",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("beta service script lacks %q", want)
		}
	}
}

// The installer and the Go renderer are separate copies of one template. These
// are the static directives both must carry; a change to one without the other
// is drift that only shows up on a live host.
func TestOpenRCRenderMatchesInstallerDriftMarkers(t *testing.T) {
	raw, err := os.ReadFile("../../scripts/Provider_Install_Linux.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := strings.ReplaceAll(string(raw), "\r\n", "\n")
	for _, marker := range []string{
		"#!/sbin/openrc-run",
		`command_args="provide"`,
		`supervisor="supervise-daemon"`,
		"export URNETWORK_INIT=openrc",
		`retry="TERM/40/KILL/5"`,
		"respawn_delay=5",
		"respawn_max=10",
		"respawn_period=3600",
		"default_stop",
		"use net",
		"after firewall",
	} {
		if !strings.Contains(installer, marker) {
			t.Errorf("Provider_Install_Linux.sh no longer carries %q; the Go renderer and the installer have drifted", marker)
		}
	}
}

// The install-service defaults are the paths the installers use. A drift here
// writes a service script that points at a binary the installer never created,
// and every start fails with "not found" on a host that looks correctly
// installed.
func TestServiceInstallDefaultsMatchInstallers(t *testing.T) {
	for _, want := range []struct{ goos, path, root, script string }{
		{"linux", openrcServiceBinaryPath, "/usr/local/lib/urnetwork-provider", "../../scripts/Provider_Install_Linux.sh"},
		{"freebsd", bsdServiceBinaryPath, "/usr/local/libexec/urnetwork-provider", "../../scripts/Provider_Install_FreeBSD.sh"},
	} {
		if got := defaultServiceBinary(want.goos); got != want.path {
			t.Errorf("defaultServiceBinary(%q) = %q, want %q", want.goos, got, want.path)
		}
		raw, err := os.ReadFile(want.script)
		if err != nil {
			t.Fatal(err)
		}
		// The installers build the binary path from their install root
		// (provider_bin="$install_path/bin/urnetwork"), so the root is the
		// literal to compare against.
		if !strings.Contains(string(raw), want.root) {
			t.Errorf("%s no longer installs under %s; defaultServiceBinary has drifted", want.script, want.root)
		}
	}
	if got := defaultServiceBinary("darwin"); got != "" {
		t.Errorf("defaultServiceBinary(darwin) = %q, want empty (no init script for the tool to write)", got)
	}
}

// Strict parsing matters here: --usr or a stray positional must not silently
// install a service for the default account.
func TestParseInstallServiceArgs(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		user       string
		binary     string
		wantErrSub string
	}{
		{name: "both separate", args: []string{"--user", "urnet", "--binary", "/x/urnetwork"}, user: "urnet", binary: "/x/urnetwork"},
		{name: "both equals", args: []string{"--user=urnet", "--binary=/x/urnetwork"}, user: "urnet", binary: "/x/urnetwork"},
		{name: "none", args: nil},
		{name: "missing value", args: []string{"--user"}, wantErrSub: "requires a value"},
		{name: "empty value", args: []string{"--user="}, wantErrSub: "non-empty"},
		{name: "typo", args: []string{"--usr", "urnet"}, wantErrSub: "unexpected argument"},
		{name: "positional", args: []string{"urnet"}, wantErrSub: "unexpected argument"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			user, binary, err := parseInstallServiceArgs(c.args)
			if c.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErrSub) {
					t.Fatalf("err = %v, want substring %q", err, c.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if user != c.user || binary != c.binary {
				t.Errorf("got user=%q binary=%q, want user=%q binary=%q", user, binary, c.user, c.binary)
			}
		})
	}
}

// The scripts are shell, and a syntax error means the service does not start at
// boot with no visible cause until someone reads rc's output. sh -n parses both
// without running anything.
func TestRenderedServiceScriptsParse(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this host")
	}
	for _, c := range []struct{ name, script string }{
		{"openrc", renderOpenRCServiceScript("urnetwork", openrcServiceBinaryPath, "urnet", "/home/urnet")},
		{"rcd", renderBSDServiceScript("urnetwork", bsdServiceBinaryPath, "urnet", "/home/urnet")},
	} {
		path := filepath.Join(t.TempDir(), c.name)
		if err := os.WriteFile(path, []byte(c.script), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
			t.Errorf("%s script does not parse: %v\n%s", c.name, err, out)
		}
	}
}

// Uninstall teardown must only remove the service whose script actually runs
// the provider being removed. Getting this wrong either leaves rc.conf enabling
// a deleted binary (service's own provider) or takes down a working relay (a
// bare provider on the same box).
func TestBSDRcScriptServesBinary(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "urnetwork")
	binary := "/usr/local/libexec/urnetwork-provider/bin/urnetwork"
	script := renderBSDServiceScript("urnetwork", binary, "urnet", "/home/urnet")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if !bsdRcScriptServesBinary(scriptPath, binary) {
		t.Error("the service's own binary was not recognised")
	}
	if bsdRcScriptServesBinary(scriptPath, "/usr/local/bin/some-other-provider") {
		t.Error("an unrelated provider was reported as the service's own")
	}
	if bsdRcScriptServesBinary(filepath.Join(dir, "missing"), binary) {
		t.Error("a missing rc.d script was reported as the service's own")
	}
	// A path mentioned only in a comment must not qualify: that would let a
	// stray comment make an uninstall delete an unrelated service.
	commentOnly := filepath.Join(dir, "commented")
	body := "# runs " + binary + "\nname=\"urnetwork\"\n"
	if err := os.WriteFile(commentOnly, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if bsdRcScriptServesBinary(commentOnly, binary) {
		t.Error("a path that only appears in a comment was trusted")
	}
}

func TestBSDRemoveServiceStopsDisablesAndRemoves(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "urnetwork")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	oldRun := bsdServiceRunFn
	t.Cleanup(func() { bsdServiceRunFn = oldRun })
	bsdServiceRunFn = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return nil, nil
	}

	bsdRemoveService("urnetwork", 0, scriptPath)

	want := [][]string{
		{"service", "urnetwork", "onestop"},
		{"sysrc", "-x", "urnetwork_enable", "urnetwork_user"},
	}
	if len(calls) != len(want) {
		t.Fatalf("ran %d commands, want %d: %v", len(calls), len(want), calls)
	}
	for i := range want {
		if strings.Join(calls[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("command %d = %v, want %v", i, calls[i], want[i])
		}
	}
	if _, err := os.Stat(scriptPath); !os.IsNotExist(err) {
		t.Error("the rc.d script survived the removal")
	}
}

// bsdServiceInstall is the FreeBSD half: write the rc.d script, flip the
// rc.conf knobs with sysrc, then start through the service. It is exercised on
// any platform because the rc.d script is a pure file and the privileged
// commands go through the package seam.
func TestBSDInstallServiceWritesScriptAndEnables(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "urnetwork")
	var calls [][]string
	oldRun := bsdServiceRunFn
	t.Cleanup(func() { bsdServiceRunFn = oldRun })
	bsdServiceRunFn = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return nil, nil
	}

	in := bsdServiceInstall{
		Service: "urnetwork",
		Binary:  "/usr/local/libexec/urnetwork-provider/bin/urnetwork",
		User:    "urnet",
		Home:    "/home/urnet",
		Script:  scriptPath,
	}
	if err := bsdInstallService(in, 0 /*euid: already root*/, false); err != nil {
		t.Fatalf("bsdInstallService: %v", err)
	}

	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("rc.d script was not written: %v", err)
	}
	script := string(data)
	for _, want := range []string{
		"PROVIDE: urnetwork",
		". /etc/rc.subr",
		"run_rc_command",
		`urnetwork_user="urnet"`,
		in.Binary,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("installed rc.d script lacks %q", want)
		}
	}
	fi, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	// rc runs an rc.d script only when it is executable. A 0644 file is
	// silently ignored at boot, which looks exactly like a service that was
	// never enabled.
	if fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed rc.d script mode %v is not executable", fi.Mode().Perm())
	}

	want := [][]string{
		{"sysrc", "urnetwork_enable=YES"},
		{"sysrc", "urnetwork_user=urnet"},
		{"service", "urnetwork", "onestart"},
	}
	if len(calls) != len(want) {
		t.Fatalf("ran %d commands, want %d: %v", len(calls), len(want), calls)
	}
	for i := range want {
		if strings.Join(calls[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("command %d = %v, want %v", i, calls[i], want[i])
		}
	}
}

// The internet-facing relay must never run as root. The installer refuses it
// and so must the tool: sysrc would happily write root and every boot would
// then run the provider with full privileges.
func TestBSDInstallServiceRefusesRootUser(t *testing.T) {
	dir := t.TempDir()
	for _, user := range []string{"root", ""} {
		err := bsdInstallService(bsdServiceInstall{
			Service: "urnetwork",
			Binary:  "/usr/local/libexec/urnetwork-provider/bin/urnetwork",
			User:    user,
			Home:    "/home/urnet",
			Script:  filepath.Join(dir, "urnetwork"),
		}, 0, false)
		if err == nil {
			t.Fatalf("bsdInstallService accepted service user %q", user)
		}
		if !strings.Contains(err.Error(), "root") {
			t.Errorf("error for user %q does not explain the root refusal: %v", user, err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, "urnetwork")); !os.IsNotExist(statErr) {
			t.Errorf("a refused install still wrote the rc.d script (user %q)", user)
		}
	}
}

// dry-run is documented as safe anywhere: it must not write the script or run
// a single privileged command.
func TestBSDInstallServiceDryRunChangesNothing(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "urnetwork")
	oldRun := bsdServiceRunFn
	t.Cleanup(func() { bsdServiceRunFn = oldRun })
	bsdServiceRunFn = func(name string, args ...string) ([]byte, error) {
		t.Fatalf("dry-run ran %s %v", name, args)
		return nil, nil
	}
	err := bsdInstallService(bsdServiceInstall{
		Service: "urnetwork",
		Binary:  "/usr/local/libexec/urnetwork-provider/bin/urnetwork",
		User:    "urnet",
		Home:    "/home/urnet",
		Script:  scriptPath,
	}, 0, true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if _, statErr := os.Stat(scriptPath); !os.IsNotExist(statErr) {
		t.Error("dry-run wrote the rc.d script")
	}
}
