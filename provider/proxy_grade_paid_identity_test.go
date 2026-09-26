package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// identityKey is the key reload writes into proxy.state for a credentialed
// proxy at addr (ProxySettings.Key(): address+user).
func identityKey(addr, user string) string {
	return (&connect.ProxySettings{Address: addr, Auth: &proxy.Auth{User: user, Password: "p"}}).Key()
}

// TestPaidProxyGrader_GradesCredentialedFileProxyByIdentityKey pins that the
// grader finds a credentialed file proxy when proxy.state is keyed by
// identity (address+user), the way reload writes it. Before the fix the
// desired set was keyed by bare address, so the identity-keyed entry was
// never in it and the proxy was silently never graded.
func TestPaidProxyGrader_GradesCredentialedFileProxyByIdentityKey(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	seedProbeDNSForAddress(t, addr, tableProbePassCounter.Load())

	src := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(src, []byte(addr+":u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key := identityKey(addr, "u")
	if err := writeProxyState(&ProxyState{
		Source:  src,
		Proxies: map[string]ProxyEntry{key: {ID: 7, Health: "up", Source: "file"}},
	}); err != nil {
		t.Fatal(err)
	}

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	state, _ := readProxyState()
	e, ok := state.Proxies[key]
	if !ok {
		t.Fatalf("credentialed file proxy graded under an entry we no longer read: no entry at identity key %q", key)
	}
	if !e.Graded || e.Score != 1.0 {
		t.Fatalf("credentialed file proxy must be graded under its identity key, got graded=%v score=%v", e.Graded, e.Score)
	}
	if n := connects.Load(); n != 5 {
		t.Fatalf("expected 5 CONNECTs (4 table + 1 stage-0), got %d", n)
	}
}
