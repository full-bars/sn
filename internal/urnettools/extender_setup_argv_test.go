package urnettools

import (
	"reflect"
	"strings"
	"testing"
)

func TestExtenderSetupElevationKeepsExplicitConfirmation(t *testing.T) {
	old := runExtenderSetupFn
	defer func() { runExtenderSetupFn = old }()
	for _, args := range [][]string{{"setup"}, {"setup", "--yes"}} {
		var got ExtenderSetupOptions
		runExtenderSetupFn = func(opts ExtenderSetupOptions) error { got = opts; return nil }
		if err := cmdExtenderSetup(args); err != nil {
			t.Fatal(err)
		}
		want := append([]string{"extender"}, args...)
		if !reflect.DeepEqual(got.Argv, want) {
			t.Fatalf("argv = %q, want %q", got.Argv, want)
		}
	}
}

func TestExtenderSetupManualCommandKeepsArguments(t *testing.T) {
	opts := ExtenderSetupOptions{Argv: []string{"extender", "setup", "--state-dir", "/home/example/a b", "--user", "alice"}}
	got := formatSetupSudoCommand(opts)
	if !strings.Contains(got, "extender setup --state-dir '/home/example/a b' --user alice") {
		t.Fatalf("manual command lost arguments or shell quoting: %q", got)
	}
}

func TestExtenderSetupManualCommandDoesNotCarryApprovalFlagOnItsOwn(t *testing.T) {
	// When opts.Yes or opts.Force is true, but Argv has no approval flag,
	// the manual command must not add an approval flag across elevation on its own.
	opts := ExtenderSetupOptions{Yes: true, Force: true, Argv: []string{"extender", "setup", "--user", "alice"}}
	got := formatSetupSudoCommand(opts)
	if strings.Contains(got, "--yes") || strings.Contains(got, "-y") || strings.Contains(got, "--force") || strings.Contains(got, "-f") {
		t.Fatalf("manual command carried approval flag across elevation on its own: %q", got)
	}
}

func TestExtenderSetupWindowsManualCommandKeepsArguments(t *testing.T) {
	opts := ExtenderSetupOptions{Argv: []string{"extender", "setup", "--user", "example user"}}
	got := formatSetupPowerShellCommand(opts)
	if !strings.Contains(got, "extender setup --user 'example user'") {
		t.Fatalf("powershell manual command lost arguments or quoting: %q", got)
	}
}
