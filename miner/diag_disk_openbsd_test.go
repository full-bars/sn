package miner

import "testing"

func TestOpenBSDDiskFreeBytes(t *testing.T) {
	if _, err := diskFreeBytes(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := diskFreeBytes("/nonexistent/urnetwork-openbsd-test"); err == nil {
		t.Fatal("expected error for nonexistent path")
	}
	if got := openbsdAvailableBytes(-1, 4096); got != 0 {
		t.Fatalf("negative available blocks: got %d", got)
	}
	if got := openbsdAvailableBytes(2, 4096); got != 8192 {
		t.Fatalf("available bytes: got %d", got)
	}
}
