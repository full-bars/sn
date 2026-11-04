package miner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJWTStoreFailedRenameIsNotLoggedAsSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	s := newClientJWTStore(path)
	out := captureTlog(t, func() {
		if err := s.Put("x", clientJWTEntry{MintedAt: time.Now()}); err == nil {
			t.Fatal("expected rename failure")
		}
	})
	if strings.Contains(out, "client_jwt_store: save") || s.flushes != 0 || s.attempts != 1 {
		t.Fatalf("failed rename counted as save: attempts=%d flushes=%d log=%s", s.attempts, s.flushes, out)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	out = captureTlog(t, func() {
		if err := s.Put("x", clientJWTEntry{MintedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "attempts=2 completed=1") {
		t.Fatalf("failed attempt missing from counters: %s", out)
	}
}

func TestJWTStoreSaveTimingAfterRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store")
	s := newClientJWTStore(path)
	calls := 0
	s.clock = func() time.Time {
		calls++
		if calls == 2 {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("completion clock ran before rename: %v", err)
			}
		}
		return time.Unix(100, int64(calls)*int64(time.Millisecond))
	}
	out := captureTlog(t, func() {
		if err := s.Put("x", clientJWTEntry{MintedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "duration=1ms") {
		t.Fatalf("missing duration: %s", out)
	}
	if !strings.Contains(out, "attempts=1 completed=1") {
		t.Fatalf("missing distinct counters: %s", out)
	}
}

func TestJWTStoreSkipsAreNotSuppressed(t *testing.T) {
	s := newClientJWTStore(filepath.Join(t.TempDir(), "store"))
	e := clientJWTEntry{MintedAt: time.Now()}
	if err := s.Put("x", e); err != nil {
		t.Fatal(err)
	}
	out := captureTlog(t, func() {
		for i := 0; i < 3; i++ {
			if err := s.Put("x", e); err != nil {
				t.Fatal(err)
			}
		}
	})
	if n := strings.Count(out, "skip write"); n != 3 {
		t.Fatalf("got %d skip logs, want 3: %s", n, out)
	}
}
