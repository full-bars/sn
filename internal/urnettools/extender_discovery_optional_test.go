package urnettools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoveryOptionalSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "ext")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	startAuditSocket(t, filepath.Join(dir, "provider.sock"), func(controlRequest) string {
		return `{"ok":true,"extender":{"known":true,"discovery":{"state":"running","active_records":0,"messages_accepted":3,"last_accepted_unix_milli":12}}}`
	})
	r, err := queryExtenderFromSocket(Provider{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	d := r.Discovery
	if d == nil || d.State != "running" || d.ActiveRecords == nil || *d.ActiveRecords != 0 || d.MessagesAccepted == nil || *d.MessagesAccepted != 3 || d.LastAcceptedUnixMilli == nil || *d.LastAcceptedUnixMilli != 12 {
		t.Fatalf("discovery not preserved: %+v", d)
	}
}
