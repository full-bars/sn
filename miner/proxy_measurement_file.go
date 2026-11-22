package miner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	measurementFileMaxEntries = 65535
	measurementFileMaxBytes   = 64 << 20
)

// IPv6 measurements are independent of the platform TLS ledger.
type measurementIPv6 struct {
	At      time.Time `json:"at"`
	TCPExit string    `json:"tcp_exit,omitempty"`
	UDPExit string    `json:"udp_exit,omitempty"`
	Relay   bool      `json:"relay"`
}

type measurementFileEntry struct {
	Key     string           `json:"key"`
	Status  proxyUDPStatus   `json:"status"`
	At      time.Time        `json:"at"`
	TCPExit string           `json:"tcp_exit,omitempty"`
	UDPExit string           `json:"udp_exit,omitempty"`
	IPv6    *measurementIPv6 `json:"ipv6,omitempty"`
}

type measurementFile struct {
	Version int                    `json:"version"`
	Entries []measurementFileEntry `json:"entries"`
}

func measurementKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func validateMeasurementEntries(entries []measurementFileEntry) error {
	if len(entries) > measurementFileMaxEntries {
		return errors.New("too many measurements")
	}
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		key, err := hex.DecodeString(e.Key)
		if err != nil || len(key) != sha256.Size || hex.EncodeToString(key) != e.Key || e.Status < proxyUDPUnknown || e.Status > proxyUDPExitMismatch || e.At.IsZero() {
			return errors.New("invalid measurement")
		}
		if _, exists := seen[e.Key]; exists {
			return errors.New("duplicate measurement")
		}
		seen[e.Key] = struct{}{}
		validIP := func(s string) bool { return s == "" || net.ParseIP(s) != nil }
		if !validIP(e.TCPExit) || !validIP(e.UDPExit) {
			return errors.New("invalid exit address")
		}
		if e.IPv6 != nil && (e.IPv6.At.IsZero() || !validIP(e.IPv6.TCPExit) || !validIP(e.IPv6.UDPExit)) {
			return errors.New("invalid IPv6 measurement")
		}
	}
	return nil
}

func loadMeasurementFile(path string) ([]measurementFileEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, measurementFileMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > measurementFileMaxBytes {
		return nil, errors.New("measurement file too large")
	}
	var state measurementFile
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, err
	}
	if state.Version != 1 {
		return nil, fmt.Errorf("unsupported measurement version %d", state.Version)
	}
	if err := validateMeasurementEntries(state.Entries); err != nil {
		return nil, err
	}
	return state.Entries, nil
}

func saveMeasurementFile(path string, entries []measurementFileEntry) error {
	if err := validateMeasurementEntries(entries); err != nil {
		return err
	}
	entries = append([]measurementFileEntry{}, entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	b, err := json.Marshal(measurementFile{Version: 1, Entries: entries})
	if err != nil {
		return err
	}
	if len(b) > measurementFileMaxBytes {
		return errors.New("measurement file too large")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".measurements-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
