package provider

// DESIGN ADAPTATION: These functions were in the fork's connect library
// (ip_probe_targets_api.go). v2026 removed them. The stubs return
// reasonable defaults based on the fork's probe table size.

import (
	"os"
	"path/filepath"
)

// probeHostCount returns the size of the probe host table.
// Fork default: ~200 hosts in the health-class table.
func probeHostCount() int { return 200 }

// sampleProbeTargets returns one pass's worth of targets for probing.
// Stub returns a single dummy host; real implementation requires the probe table.
func sampleProbeTargets(seed uint64, n int) (hosts []string, resolver string) {
	hosts = make([]string, n)
	for i := range hosts {
		hosts[i] = "probe.invalid"
	}
	resolver = "dns.invalid"
	return
}

// atomicWriteFile writes data to a file atomically using a temp file + rename.
// Real implementation ported from fork main.go:678.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
