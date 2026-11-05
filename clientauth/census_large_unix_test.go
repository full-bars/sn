//go:build linux || darwin

package clientauth

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNetworkTokenCensusLargeDirectory(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 6000; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("slot-%04d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "jwt")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(dir, networkRenewalLeftoverPrefix("jwt")+"leftover")
	if err := os.WriteFile(leftover, []byte("unused"), 0600); err != nil {
		t.Fatal(err)
	}
	if ok, err := RenewNetworkToken(path, "old", "new"); err != nil || !ok {
		t.Fatalf("renew: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("leftover still exists: %v", err)
	}
	prior := filepath.Join(dir, networkTokenQuarantineName("jwt", time.Unix(100, 0), 1))
	if err := os.WriteFile(prior, []byte("prior"), 0600); err != nil {
		t.Fatal(err)
	}
	q, ok, err := QuarantineNetworkToken(path, "new", time.Unix(101, 0))
	if err != nil || !ok || q.Consecutive != 2 {
		t.Fatalf("quarantine: %#v ok=%v err=%v", q, ok, err)
	}
	store, err := openRegistrationStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.names(1, "jwt.rejected-"); err == nil {
		t.Fatal("matching names exceeded the bound")
	}
	if err := store.directory.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.names(4096, "jwt.rejected-"); err == nil {
		t.Fatal("closed directory accepted")
	}
	_ = store.close()
}
