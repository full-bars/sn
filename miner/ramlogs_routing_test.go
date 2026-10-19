package miner

import "testing"

// The ramlog redirect is the one logging decision made from init(), before
// any subcommand runs, and it cannot be re-made later: these tests pin the
// decision table. The default is stderr; only a long-running
// provide/auth-provide invocation with URNETWORK_RAMLOGS=1 or the lowmem/eco
// profile redirects, so the select lines ([net][s]select) and the rest of the
// pipeline stay on the terminal, docker logs or the journal everywhere else.

func TestIsLongRunningSubcommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"provide", []string{"miner", "provide"}, true},
		{"provide with flags", []string{"miner", "provide", "--port=1234"}, true},
		{"auth-provide", []string{"miner", "auth-provide", "--user_auth=u"}, true},
		{"proxy subcommand", []string{"miner", "proxy", "summary"}, false},
		{"proxy remove-dead", []string{"miner", "proxy", "remove-dead", "--yes"}, false},
		{"no subcommand", []string{"miner"}, false},
		{"provide --help", []string{"miner", "provide", "--help"}, false},
		{"provide -h", []string{"miner", "provide", "-h"}, false},
		{"auth-provide --version", []string{"miner", "auth-provide", "--version"}, false},
		{"help among other flags", []string{"miner", "provide", "--port=1", "-h"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLongRunningSubcommand(tt.args); got != tt.want {
				t.Fatalf("isLongRunningSubcommand(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestRamlogsRequested(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		ramlogs string
		want    bool
	}{
		{"default is off", "", "", false},
		{"ramlogs=1 enables", "", "1", true},
		{"ramlogs=0 does not", "", "0", false},
		{"lowmem enables", "lowmem", "", true},
		{"eco enables", "eco", "", true},
		{"other profile does not", "turbo-v4", "", false},
		// The v33 rule is an OR: a profile that carries the ramlog cannot be
		// turned off with the env var.
		{"lowmem with ramlogs=0 stays on", "lowmem", "0", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("URNETWORK_PROFILE", tt.profile)
			t.Setenv("URNETWORK_RAMLOGS", tt.ramlogs)
			if got := ramlogsRequested(); got != tt.want {
				t.Fatalf("ramlogsRequested() with profile=%q ramlogs=%q = %v, want %v",
					tt.profile, tt.ramlogs, got, tt.want)
			}
		})
	}
}

func TestRamlogRedirectRequested(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		profile string
		ramlogs string
		want    bool
	}{
		{"default provide stays on stderr", []string{"miner", "provide"}, "", "", false},
		{"provide with ramlogs redirects", []string{"miner", "provide"}, "", "1", true},
		{"auth-provide with ramlogs redirects", []string{"miner", "auth-provide"}, "", "1", true},
		{"lowmem provide redirects", []string{"miner", "provide"}, "lowmem", "", true},
		{"eco auth-provide redirects", []string{"miner", "auth-provide"}, "eco", "", true},
		{"one-shot subcommand keeps the terminal", []string{"miner", "proxy", "summary"}, "", "1", false},
		{"one-shot wallet command keeps the terminal", []string{"miner", "wallet", "set"}, "lowmem", "1", false},
		{"provide --help keeps the terminal", []string{"miner", "provide", "--help"}, "", "1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("URNETWORK_PROFILE", tt.profile)
			t.Setenv("URNETWORK_RAMLOGS", tt.ramlogs)
			if got := ramlogRedirectRequested(tt.args); got != tt.want {
				t.Fatalf("ramlogRedirectRequested(%q) with profile=%q ramlogs=%q = %v, want %v",
					tt.args, tt.profile, tt.ramlogs, got, tt.want)
			}
		})
	}
}
