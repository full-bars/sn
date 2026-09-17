//go:build ignore

package provider

import (
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestNextProxyID_MonotonicallyIncreasing(t *testing.T) {
	atomic.StoreInt64(&proxyIDCounter, 0)

	id0 := nextProxyID()
	id1 := nextProxyID()
	id2 := nextProxyID()

	if id0 != 0 || id1 != 1 || id2 != 2 {
		t.Fatalf("expected 0,1,2 got %d,%d,%d", id0, id1, id2)
	}
}

func TestInitProxyIDCounter_StartsAboveExisting(t *testing.T) {
	atomic.StoreInt64(&proxyIDCounter, 0)
	initProxyIDCounter(10)
	id := nextProxyID()
	if id != 11 {
		t.Fatalf("expected first ID after init to be 11, got %d", id)
	}
}

func TestInitProxyIDCounter_NoopIfAlreadyHigher(t *testing.T) {
	atomic.StoreInt64(&proxyIDCounter, 100)
	initProxyIDCounter(5)
	id := nextProxyID()
	if id != 100 {
		t.Fatalf("expected counter unchanged at 100, got %d", id)
	}
}

func TestCurrentProxyIDCounter(t *testing.T) {
	atomic.StoreInt64(&proxyIDCounter, 42)
	if got := currentProxyIDCounter(); got != 42 {
		t.Fatalf("expected 42, got %d", got)
	}
}

func TestWriteReadProxyState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.state")

	s := &ProxyState{
		Source:    "/app/proxy.txt",
		StartedAt: time.Now().Truncate(time.Second),
		NextID:    5,
		Proxies: map[string]ProxyEntry{
			"1.2.3.4:1080": {ID: 0, Health: "up"},
			"5.6.7.8:1080": {ID: 1, Health: "dead"},
		},
	}

	if err := writeProxyStateTo(path, s); err != nil {
		t.Fatal(err)
	}

	got, err := readProxyStateFrom(path)
	if err != nil {
		t.Fatal(err)
	}

	if got.Source != s.Source {
		t.Errorf("source: got %q want %q", got.Source, s.Source)
	}
	if got.NextID != s.NextID {
		t.Errorf("nextID: got %d want %d", got.NextID, s.NextID)
	}
	if len(got.Proxies) != 2 {
		t.Errorf("proxies: got %d want 2", len(got.Proxies))
	}
}

func TestReadProxyState_NotExist(t *testing.T) {
	s, err := readProxyStateFrom("/tmp/does-not-exist-proxy.state")
	if err != nil {
		t.Fatal(err)
	}
	if s.Proxies == nil {
		t.Fatal("expected non-nil Proxies map")
	}
}

func TestResolveProxyID_ExistingAddressKeepsID(t *testing.T) {
	s := &ProxyState{
		Proxies: map[string]ProxyEntry{
			"1.2.3.4:1080": {ID: 42},
		},
	}
	atomic.StoreInt64(&proxyIDCounter, 100)
	id := resolveProxyID(s, "1.2.3.4:1080")
	if id != 42 {
		t.Fatalf("expected existing ID 42, got %d", id)
	}
}

func TestResolveProxyID_NewAddressGetsNextID(t *testing.T) {
	s := &ProxyState{
		Proxies: map[string]ProxyEntry{},
	}
	atomic.StoreInt64(&proxyIDCounter, 50)
	id := resolveProxyID(s, "2.3.4.5:1080")
	if id != 50 {
		t.Fatalf("expected new ID 50, got %d", id)
	}
	if s.Proxies["2.3.4.5:1080"].ID != 50 {
		t.Fatalf("expected stored ID 50, got %d", s.Proxies["2.3.4.5:1080"].ID)
	}
}

func TestTagProxySourceIfUnset(t *testing.T) {
	s := &ProxyState{
		Proxies: map[string]ProxyEntry{
			"1.2.3.4:1080": {ID: 1, Source: "file"},
			"5.6.7.8:1080": {ID: 2},
		},
	}
	tagProxySourceIfUnset(s, "1.2.3.4:1080", "url")
	if s.Proxies["1.2.3.4:1080"].Source != "file" {
		t.Fatalf("expected source unchanged as 'file', got %q", s.Proxies["1.2.3.4:1080"].Source)
	}

	tagProxySourceIfUnset(s, "5.6.7.8:1080", "url")
	if s.Proxies["5.6.7.8:1080"].Source != "url" {
		t.Fatalf("expected source set to 'url', got %q", s.Proxies["5.6.7.8:1080"].Source)
	}
}

func TestWriteProxyStateTo_MkdirAllError(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "subdir", "proxy.state")

	err := writeProxyStateTo(path, &ProxyState{Proxies: map[string]ProxyEntry{}})
	if err == nil {
		t.Fatal("expected an error when the parent directory cannot be created (path component is a file)")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("proxy.state must not exist after a failed write")
	}
}

func TestWriteProxyStateTo_CreateTempError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits don't apply on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permission bits")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "state_dir")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0700) })

	path := filepath.Join(sub, "proxy.state")
	err := writeProxyStateTo(path, &ProxyState{Proxies: map[string]ProxyEntry{}})
	if err == nil {
		t.Fatal("expected CreateTemp to fail in a directory without write permission")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("proxy.state must not exist after a failed write")
	}
}

func TestWriteProxyStateTo_HappyPathStillWorks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "proxy.state")
	want := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.2.3.4:1080": {ID: 1, Health: "up"},
	}}
	if err := writeProxyStateTo(path, want); err != nil {
		t.Fatalf("unexpected error on the happy path: %v", err)
	}
	got, err := readProxyStateFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Proxies["1.2.3.4:1080"].ID != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestProxyStatePath(t *testing.T) {
	p, err := proxyStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p) != "proxy.state" {
		t.Fatalf("expected proxy.state, got %s", p)
	}
}
