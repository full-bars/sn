//go:build linux

package miner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMinerProcResourcesParsing(t *testing.T) {
	threads := minerProcField([]byte("Name: x\nThreads: 7\n"), "Threads:", 1)
	if threads == nil || *threads != 7 || minerProcField([]byte("Threads: bad\n"), "Threads:", 1) != nil {
		t.Fatal("thread parsing failed")
	}
	used, total := minerSwap([]byte("SwapTotal: 8 kB\nSwapFree: 3 kB\n"))
	if used == nil || total == nil || *used != 5120 || *total != 8192 {
		t.Fatalf("swap = %v %v", used, total)
	}
	for _, data := range []string{"SwapTotal: 0 kB\n", "SwapTotal: 1 kB\nSwapFree: 2 kB\n", "SwapTotal: 18446744073709551615 kB\nSwapFree: 0 kB\n"} {
		if used, total := minerSwap([]byte(data)); used != nil || total != nil {
			t.Fatalf("invalid swap was reported: %q", data)
		}
	}
	used, total = minerSwap([]byte("SwapTotal: 0 kB\nSwapFree: 0 kB\n"))
	if used == nil || total == nil || *used != 0 || *total != 0 {
		t.Fatal("reported zero swap was lost")
	}
}

func TestMinerPidsLimitAncestors(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "group", "child")
	if err := os.MkdirAll(child, 0700); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{filepath.Join(root, "pids.max"): "200", filepath.Join(root, "group", "pids.max"): "25", filepath.Join(child, "pids.max"): "max"} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	got := minerPidsLimit([]byte("0::/group/child\n"), []string{root})
	if got == nil || *got != 25 {
		t.Fatalf("pids limit = %v", got)
	}
	if minerPidsLimit([]byte("2:memory:/group/child\n"), []string{root}) != nil {
		t.Fatal("used unrelated controller")
	}
}

func TestMinerResourcesJSONPresence(t *testing.T) {
	zero := uint64(0)
	data, err := json.Marshal(snapshotResources{Threads: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"threads":0`) || strings.Contains(string(data), `"swap_used_bytes"`) {
		t.Fatalf("presence: %s", data)
	}
	resources := collectMinerResources()
	if resources.Threads == nil || *resources.Threads == 0 {
		t.Fatal("live thread count unavailable")
	}
}
