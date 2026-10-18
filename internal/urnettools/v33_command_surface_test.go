package urnettools

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// v33ClientCommand is one row of the v33 (3.23-fix) urnet-tools command
// surface, read from the reference client at 9bdb0909a and recorded as the
// 47-row inventory in the parity audit (scratch/sn-parity/D.md, section 1).
// This table is the contract the sn fork's client must keep: a command or
// alias named here that disappears from the Cobra tree is a surface
// regression, and this test is what makes the loss loud.
type v33ClientCommand struct {
	name    string
	aliases []string
	// subs are the commands registered as Cobra children (the wallet group).
	// Commands that dispatch subcommands from raw arguments are covered by
	// the surface checks below instead.
	subs   []string
	hidden bool
}

var v33ClientCommands = []v33ClientCommand{
	{name: "providers", aliases: []string{"list", "ps"}},
	{name: "status"},
	{name: "top"},
	{name: "sn-status"},
	{name: "start"},
	{name: "stop"},
	{name: "restart"},
	{name: "update"},
	{name: "idle-update"},
	{name: "self-update", aliases: []string{"selfupdate"}},
	{name: "logs"},
	{name: "summary"},
	{name: "version"},
	{name: "default"},
	{name: "session"},
	{name: "turbo"},
	{name: "auto"},
	{name: "eco"},
	{name: "lowmode"},
	{name: "ramlogs"},
	{name: "optimize"},
	{name: "hotswap", aliases: []string{"hot-swap"}},
	{name: "fast-auth", aliases: []string{"fastauth"}},
	{name: "get", aliases: []string{"show"}},
	{name: "set"},
	{name: "rename", aliases: []string{"set-node-name"}},
	{name: "auth"},
	{name: "choose-network", aliases: []string{"choose_network"}},
	{name: "wallet", subs: []string{"status", "challenge", "set", "sign"}},
	{name: "proxy"},
	{name: "report"},
	{name: "reinstall"},
	{name: "uninstall"},
	{name: "auto-update", aliases: []string{"autoupdate"}},
	{name: "auto-start", aliases: []string{"autostart"}},
	{name: "self-heal", aliases: []string{"selfheal"}},
	{name: "show-ip", aliases: []string{"ip-detect", "ipdetect"}},
	{name: "history", aliases: []string{"audit"}},
	{name: "autopilot"},
	{name: "enforcement"},
	{name: "baseline"},
	{name: "metrics"},
	{name: "smart-dialer", aliases: []string{"smart_dialer"}},
	{name: "profile", aliases: []string{"profiles"}},
	{name: "dashboard", aliases: []string{"dash", "panel"}},
	{name: "config"},
	{name: "usage"},
	{name: "__do-restart", hidden: true},
}

// TestV33CommandSurfaceParity fails when any command or alias in the v33
// inventory is missing from the client tree. `urtop` is deliberately absent:
// it is an install-time link name for the binary, not a Cobra alias.
func TestV33CommandSurfaceParity(t *testing.T) {
	root := buildRootCmd()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	for _, want := range v33ClientCommands {
		found, _, err := root.Find([]string{want.name})
		if err != nil || found.Name() != want.name {
			t.Errorf("v33 command %q is missing from the client tree (err=%v)", want.name, err)
			continue
		}
		if found.Hidden != want.hidden {
			t.Errorf("v33 command %q hidden=%v, want %v", want.name, found.Hidden, want.hidden)
		}
		for _, alias := range want.aliases {
			resolved, _, err := root.Find([]string{alias})
			if err != nil || resolved.Name() != want.name {
				t.Errorf("v33 alias %q does not resolve to %q (err=%v)", alias, want.name, err)
			}
		}
		for _, sub := range want.subs {
			child, _, err := root.Find([]string{want.name, sub})
			if err != nil || child.Name() != sub {
				t.Errorf("v33 subcommand %q %q is missing (err=%v)", want.name, sub, err)
			}
		}
	}
}

// v33DockerCommands is the v33 (3.23-fix) urnet-docker command surface, read
// from the reference client at 9bdb0909a and recorded as the D1-D22 inventory
// in the parity audit (sn-parity/D.md, section 2). The docker shim is the
// other half of the same client, and it forwards to the same provider
// commands, so its surface gets the same regression guard as urnet-tools.
var v33DockerCommands = []v33ClientCommand{
	{name: "providers", aliases: []string{"list", "ps"}},
	{name: "status"},
	{name: "top"},
	{name: "sn-status"},
	{name: "start"},
	{name: "stop"},
	{name: "restart"},
	{name: "logs"},
	{name: "auth"},
	{name: "choose-network", aliases: []string{"choose_network"}},
	{name: "summary"},
	{name: "report"},
	{name: "update", aliases: []string{"self-update", "selfupdate"}},
	{name: "idle-update"},
	{name: "version"},
	{name: "proxy", subs: []string{"add", "paste", "clear", "remove", "add-source", "remove-source", "refresh", "remove-dead", "health", "traffic", "summary", "trim", "audit", "exclude"}},
	{name: "self-heal", aliases: []string{"selfheal"}},
	{name: "set"},
	{name: "rename"},
	{name: "fast-auth", aliases: []string{"fastauth"}},
	{name: "session"},
	{name: "exec"},
	{name: "direct"},
	{name: "usage"},
}

// TestV33DockerCommandSurfaceParity mirrors TestV33CommandSurfaceParity for
// the urnet-docker tree: a command or alias in the D1-D22 inventory that
// disappears is a surface regression.
func TestV33DockerCommandSurfaceParity(t *testing.T) {
	root := buildDockerRootCmd()
	root.InitDefaultHelpCmd()

	for _, want := range v33DockerCommands {
		found, _, err := root.Find([]string{want.name})
		if err != nil || found.Name() != want.name {
			t.Errorf("v33 urnet-docker command %q is missing from the tree (err=%v)", want.name, err)
			continue
		}
		for _, alias := range want.aliases {
			resolved, _, err := root.Find([]string{alias})
			if err != nil || resolved.Name() != want.name {
				t.Errorf("v33 urnet-docker alias %q does not resolve to %q (err=%v)", alias, want.name, err)
			}
		}
		for _, sub := range want.subs {
			child, _, err := root.Find([]string{want.name, sub})
			if err != nil || child.Name() != sub {
				t.Errorf("v33 urnet-docker subcommand %q %q is missing (err=%v)", want.name, sub, err)
			}
		}
	}
}

// The v33 proxy group dispatches its subcommands from raw arguments, so the
// tree cannot be walked; the dispatcher's own required-subcommand list is the
// surface. Every v33 proxy verb must still be named there.
func TestV33ProxySubcommandSurface(t *testing.T) {
	err := cmdProxy(nil, false, true)
	if err == nil {
		t.Fatal("bare `proxy` must report the subcommand surface")
	}
	for _, sub := range []string{"add", "paste", "clear", "remove", "refresh", "add-source", "remove-source", "health", "traffic", "ids", "remove-dead", "trim", "audit"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("proxy subcommand %q is no longer named by the dispatcher: %v", sub, err)
		}
	}
}

// The v33 client enabled Cobra's shell completion command; the sn port had
// disabled it (CompletionOptions.DisableDefaultCmd = true). It must exist in
// the tree and actually emit a completion script. This client routes Cobra's
// own output to stderr (rootCmd.SetOut(os.Stderr) in buildRootCmd), so the
// script arrives on stderr — the same stream v33 writes it to, since it keeps
// the identical routing.
func TestCompletionCommandEnabled(t *testing.T) {
	root := buildRootCmd()
	root.InitDefaultCompletionCmd()
	if c, _, err := root.Find([]string{"completion"}); err != nil || c.Name() != "completion" {
		t.Fatalf("completion command is missing from the tree (err=%v)", err)
	}
	out := captureStderr(t, func() {
		if err := Run([]string{"completion", "bash"}); err != nil {
			t.Fatalf("urnet-tools completion bash: %v", err)
		}
	})
	if !strings.Contains(out, "bash completion V2 for urnet-tools") {
		t.Errorf("completion bash output does not look like a completion script:\n%s", out)
	}
}

// TestDocsReferenceExistingUrnetToolsCommands extends the docs-reference
// guard the repo already keeps for the provider CLI (miner/cli_command_refs_test.go)
// to the urnet-tools client: every `urnet-tools <command>` named in the docs
// must be a command the client tree actually has, so a removed command cannot
// stay documented and send operators to something that will not run.
func TestDocsReferenceExistingUrnetToolsCommands(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "docs", "*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no docs found to check: %v", err)
	}
	ref := regexp.MustCompile(`urnet-tools ([a-z][a-z0-9-]*)`)
	// Builtins that are not per-provider commands: completion is Cobra's and
	// help is handled by the framework.
	builtin := map[string]bool{"completion": true, "help": true}

	root := buildRootCmd()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	missing := map[string][]string{}
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ref.FindAllStringSubmatch(string(b), -1) {
			name := m[1]
			if builtin[name] {
				continue
			}
			if _, _, err := root.Find([]string{name}); err != nil {
				missing[name] = append(missing[name], filepath.Base(file))
			}
		}
	}
	for name, where := range missing {
		t.Errorf("docs reference `urnet-tools %s` but the client has no such command (in %s)", name, strings.Join(where, ", "))
	}
}
