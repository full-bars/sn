package connectx

import (
	"os"
	"runtime"
	"testing"
)

func TestHostMemoryReaders(t *testing.T) {
	total, tok := HostMemoryTotalBytes()
	avail, aok := HostMemoryAvailableBytes()
	switch runtime.GOOS {
	case "openbsd":
		if aok {
			t.Fatalf("available memory unexpectedly supported: %d", avail)
		}
		// hw.physmem width can vary; an unreadable total is reported unknown.
		// The OpenBSD CI job runs on a real kernel and sets
		// URNET_REQUIRE_HOST_MEMORY, which turns an unreadable total into a
		// failure so the width question is answered by a measurement.
		if os.Getenv("URNET_REQUIRE_HOST_MEMORY") != "" && !tok {
			t.Fatalf("hw.physmem total is unreadable on this kernel")
		}
		if tok && total <= 0 {
			t.Fatalf("invalid total: %d", total)
		}
		return
	case "linux", "darwin", "windows":
	default:
		if tok || aok {
			t.Fatalf("expected unavailable on %s, got total=%v avail=%v", runtime.GOOS, tok, aok)
		}
		return
	}
	if !tok || total <= 0 {
		t.Fatalf("total unreadable: %d %v", total, tok)
	}
	if !aok || avail < 0 {
		t.Fatalf("available unreadable: %d %v", avail, aok)
	}
	if avail > total {
		t.Fatalf("available %d exceeds total %d", avail, total)
	}
}

func TestEffectiveRAMLimitUsesHostTotalWhenNoCgroup(t *testing.T) {
	v, _ := EffectiveRAMLimit()
	if v <= 0 {
		t.Fatalf("non-positive limit %d", v)
	}
}
