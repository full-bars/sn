package miner

import "testing"

// proxy_edit_reload_signal_test.go pins that every command which changes the
// stored proxy list also tells a running provider, so the change applies
// without a restart. The reloader exists for an unprofiled run and applies the
// desired set on the trigger (run.go); the commands below changed the store
// and never wrote it, so a running miner kept serving the old list until its
// next start. `proxy clear`, `proxy paste`, the source commands and the
// file-backed add already signalled.

// reloadSeqNow is what the running provider's watcher compares: the sequence
// number in the trigger file.
func reloadSeqNow(t *testing.T) int {
	t.Helper()
	path, err := proxyReloadPath()
	if err != nil {
		t.Fatal(err)
	}
	n, err := readReloadSeq(path)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func assertSignalsReload(t *testing.T, what string, run func()) {
	t.Helper()
	before := reloadSeqNow(t)
	run()
	if after := reloadSeqNow(t); after <= before {
		t.Fatalf("%s changed the stored list but did not signal a running provider (reload seq %d -> %d)", what, before, after)
	}
}

func TestProxyAddSignalsARunningProvider(t *testing.T) {
	withTempHome(t)
	assertSignalsReload(t, "proxy add", func() {
		proxyAdd(parseArgsForTest(t, []string{"proxy", "add", "192.0.2.10:1080", "-f"}))
	})
	if _, ok := readProxyConfig().Servers["192.0.2.10:1080"]; !ok {
		t.Fatal("the add itself must still land in the stored list")
	}
}

func TestProxyRemoveSignalsARunningProvider(t *testing.T) {
	withTempHome(t)
	writeProxyConfig(&ProxyConfig{Servers: map[string]string{"192.0.2.10:1080": "", "192.0.2.11:1080": ""}})
	assertSignalsReload(t, "proxy remove", func() {
		proxyRemove(parseArgsForTest(t, []string{"proxy", "remove", "192.0.2.10:1080"}))
	})
	servers := readProxyConfig().Servers
	if _, ok := servers["192.0.2.10:1080"]; ok || len(servers) != 1 {
		t.Fatalf("the removal itself must still happen, stored list is %v", servers)
	}
}

func TestProxyRemoveAllSignalsARunningProvider(t *testing.T) {
	withTempHome(t)
	seedClearableStores(t)
	assertSignalsReload(t, "proxy remove --all", func() {
		proxyRemove(parseArgsForTest(t, []string{"proxy", "remove", "--all"}))
	})
	assertStoresCleared(t)
}

func TestProxyAuthEditsSignalARunningProvider(t *testing.T) {
	withTempHome(t)
	assertSignalsReload(t, "proxy auth add", func() {
		proxyAuthAdd(parseArgsForTest(t, []string{"proxy", "auth", "add", "mykey", "alice", "secret", "-f"}))
	})
	assertSignalsReload(t, "proxy auth remove", func() {
		proxyAuthRemove(parseArgsForTest(t, []string{"proxy", "auth", "remove", "mykey"}))
	})
}

// An add that changes nothing (the operator declines to change a key) must not
// wake the running provider for no reason.
func TestProxyAddThatDeclinesDoesNotSignal(t *testing.T) {
	withTempHome(t)
	writeProxyConfig(&ProxyConfig{Servers: map[string]string{"192.0.2.10:1080": "keyA"}})
	before := reloadSeqNow(t)
	// without -f and with no input, the confirm reads EOF and declines
	proxyAdd(parseArgsForTest(t, []string{"proxy", "add", "keyB@192.0.2.10:1080"}))
	if after := reloadSeqNow(t); after != before {
		t.Fatalf("a declined add signalled a reload (seq %d -> %d)", before, after)
	}
}
