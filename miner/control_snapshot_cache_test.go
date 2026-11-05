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
