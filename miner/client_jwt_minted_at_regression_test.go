package miner

import (
	"path/filepath"
	"testing"
	"time"
)

func TestJWTStoreNewMintTimeIsPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store")
	s := newClientJWTStore(path)
	entry := clientJWTEntry{ByClientJWT: "same-token", MintedAt: time.Now().Add(-time.Hour)}
	if err := s.Put("proxy", entry); err != nil {
		t.Fatal(err)
	}
	entry.MintedAt = entry.MintedAt.Add(time.Minute)
	if err := s.Put("proxy", entry); err != nil {
		t.Fatal(err)
	}
	got, ok := newClientJWTStore(path).Get("proxy")
	if !ok || !got.MintedAt.Equal(entry.MintedAt) || s.flushes != 2 {
		t.Fatalf("mint time not persisted: got=%v ok=%v writes=%d", got.MintedAt, ok, s.flushes)
	}
}
