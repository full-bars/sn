//go:build linux

package urnettools

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifySocketPeerBinding(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("unix", filepath.Join(dir, "provider.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				var req controlRequest
				if json.NewDecoder(c).Decode(&req) == nil {
					_ = json.NewEncoder(c).Encode(controlResponse{OK: true, BuildVersion: "wanted"})
				}
			}()
		}
	}()
	p := Provider{StateDir: dir}
	if version, ok := providerVersionFromSocketForPID(p, os.Getpid()+1); ok {
		t.Fatalf("accepted other pid's version %q", version)
	}
	if version, ok := providerVersionFromSocketForPID(p, os.Getpid()); !ok || version != "wanted" {
		t.Fatalf("legitimate peer: %q %v", version, ok)
	}
	stubVerifyLoop(t)
	p.PID = os.Getpid() + 1
	verifyDiscoverFn = func() []Provider { return []Provider{{StateDir: dir, PID: os.Getpid()}} }
	verifyRunningImageHandleFn = func(int) (string, error) { return "", os.ErrPermission }
	verifySocketVersionFn = providerVersionFromSocketForPID
	if err := verifyRestartLoop(p, testConfig("wrong"), true, ""); err == nil {
		t.Fatal("accepted wrong version from authenticated peer")
	}
	if err := verifyRestartLoop(p, testConfig("wanted"), true, ""); err != nil {
		t.Fatalf("matching peer rejected: %v", err)
	}
}
