package miner

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestProxyClearHelperProcess runs `proxy clear` in a child process because a failed clear
// ends the command with os.Exit. GO_WANT_CLEAR_HELPER gates it so a normal
// package run skips it.
func TestProxyClearHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_CLEAR_HELPER") != "1" {
		t.Skip("helper process only")
	}
	proxyClear(parseArgsForTest(t, []string{"proxy", "clear"}))
}

// An unreadable proxy.state must not hide the failure or the reload: the other
// stores were already cleared, so a running provider has to re-read them, and
// the operator has to be told proxy.state was left as it was.
func TestProxyClearReloadsAndReportsWhenStateUnreadable(t *testing.T) {
	withTempHome(t)
	seedClearableStores(t)
	statePath, err := proxyStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestProxyClearHelperProcess$")
	cmd.Env = append(os.Environ(), "GO_WANT_CLEAR_HELPER=1")
	out, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 77 {
		t.Fatalf("proxy clear exit = %v, want exit 77\n%s", err, out)
	}
	if !strings.Contains(string(out), "proxy.state not cleared") {
		t.Fatalf("the state read error was not surfaced:\n%s", out)
	}

	if cfg := readProxyConfig(); len(cfg.Servers) != 0 {
		t.Fatalf("internal config not cleared: %v", cfg.Servers)
	}
	if urlState, err := readProxyURLState(); err != nil || len(urlState.Cache) != 0 {
		t.Fatalf("URL cache not cleared past the unreadable state: %v %v", urlState, err)
	}
	reloadPath, err := proxyReloadPath()
	if err != nil {
		t.Fatal(err)
	}
	if seq, err := readReloadSeq(reloadPath); err != nil || seq != 1 {
		t.Fatalf("reload trigger seq = %d (%v), want 1", seq, err)
	}
}
