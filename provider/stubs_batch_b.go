package provider

import (
	"os"
	"path/filepath"
)

// probeHostCount returns the number of probe host targets.
func probeHostCount() int { return 200 }

// sampleProbeTargets returns a deterministic sample of n probe host
// names and the DNS resolver address.
func sampleProbeTargets(seed uint64, n int) (hosts []string, resolver string) {
	resolver = "1.1.1.1"
	if n <= 0 {
		return nil, resolver
	}
	hosts = make([]string, n)
	for i := range hosts {
		hosts[i] = "probe.invalid"
	}
	return hosts, resolver
}

// atomicWriteFile writes data to path atomically via a temp file + rename.
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
	if err := os.Chmod(tmp, perm); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// gradeTier maps a 0-1 score to an A-F letter grade.
func gradeTier(score float64) string {
	switch {
	case score >= 0.9:
		return "A"
	case score >= 0.75:
		return "B"
	case score >= 0.5:
		return "C"
	case score >= 0.25:
		return "D"
	default:
		return "F"
	}
}
