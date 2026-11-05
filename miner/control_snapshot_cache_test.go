package miner

import (
	"errors"
	"testing"
	"time"
)

func TestSnapshotCollectorCache(t *testing.T) {
	calls := 0
	c := snapshotCollector{build: func() (*nodeSnapshot, error) {
		calls++
		return &nodeSnapshot{}, nil
	}}
	now := time.Now()
	first, err := c.get(now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.get(now.Add(snapshotCacheTTL - time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || first != second {
		t.Fatalf("within TTL: calls=%d, same=%v", calls, first == second)
	}
	third, err := c.get(now.Add(snapshotCacheTTL))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || third == first {
		t.Fatalf("at expiry: calls=%d, same=%v", calls, third == first)
	}
}

func TestSnapshotCollectorDoesNotCacheErrors(t *testing.T) {
	calls := 0
	c := snapshotCollector{build: func() (*nodeSnapshot, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("unavailable")
		}
		return &nodeSnapshot{}, nil
	}}
	now := time.Now()
	if _, err := c.get(now); err == nil {
		t.Fatal("expected error")
	}
	if _, err := c.get(now); err != nil || calls != 2 {
		t.Fatalf("retry: calls=%d err=%v", calls, err)
	}
}

// A control mutation changes what the snapshot reports. Without an invalidation
// the next request answers from the previous state for up to a second, so an
// operator who sets a key and immediately asks for status sees the old value.
func TestControlMutationsInvalidateTheSnapshotCache(t *testing.T) {
	builds := 0
	minerSnapshot.reset()
	original := minerSnapshot.build
	minerSnapshot.build = func() (*nodeSnapshot, error) {
		builds++
		return &nodeSnapshot{}, nil
	}
	t.Cleanup(func() {
		minerSnapshot.build = original
		minerSnapshot.reset()
	})

	now := time.Now()
	if _, err := minerSnapshot.get(now); err != nil {
		t.Fatal(err)
	}
	if _, err := minerSnapshot.get(now); err != nil {
		t.Fatal(err)
	}
	if builds != 1 {
		t.Fatalf("the cache did not serve the repeated request: builds=%d", builds)
	}

	// a mutating command must drop the cache, even when the command itself fails
	handleControlRequest(globalControlState, controlRequest{Cmd: "clear", Key: "snapshot_cache_test_absent_key"})

	if _, err := minerSnapshot.get(now); err != nil {
		t.Fatal(err)
	}
	if builds != 2 {
		t.Fatalf("the snapshot was not rebuilt after a control mutation: builds=%d", builds)
	}
}
