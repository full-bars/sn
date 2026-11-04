package miner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJWTStoreActualChangesDetermineCause(t *testing.T) {
	now := time.Now()
	entry := clientJWTEntry{ByClientJWT: "token", MintedAt: now}
	stale := clientJWTEntry{ByClientJWT: "old", MintedAt: now.Add(-clientJWTStaleAfter - time.Hour)}
	cases := []struct {
		name    string
		disk    map[string]clientJWTEntry
		upserts map[string]clientJWTEntry
		deletes []string
		compact bool
		cause   string
	}{
		{"stale", map[string]clientJWTEntry{"x": entry, "old": stale}, map[string]clientJWTEntry{"x": entry}, nil, false, "removed"},
		{"adoption", map[string]clientJWTEntry{"old": entry}, map[string]clientJWTEntry{"new": entry}, []string{"old"}, false, "added_removed"},
		{"format", map[string]clientJWTEntry{"x": entry}, map[string]clientJWTEntry{"x": entry}, nil, true, "normalized"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store")
			var b []byte
			var err error
			if tc.compact {
				b, err = json.Marshal(tc.disk)
			} else {
				b, err = json.MarshalIndent(tc.disk, "", "  ")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			s := newClientJWTStore(path)
			s.loaded = true
			s.entries = map[string]clientJWTEntry{}
			out := captureTlog(t, func() {
				if err := s.flushBatchLocked(tc.upserts, tc.deletes); err != nil {
					t.Fatal(err)
				}
			})
			if !strings.Contains(out, "cause="+tc.cause+" ") {
				t.Fatalf("want cause=%s: %s", tc.cause, out)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var entries map[string]clientJWTEntry
			if err := json.Unmarshal(got, &entries); err != nil {
				t.Fatal(err)
			}
			for key, v := range tc.upserts {
				if !entries[key].MintedAt.Equal(v.MintedAt) {
					t.Fatalf("minted_at changed for %s", key)
				}
			}
			if tc.name == "stale" {
				if _, ok := entries["old"]; ok {
					t.Fatal("stale key survived")
				}
			}
		})
	}
}
