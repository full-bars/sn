package miner

import (
	"os"
	"path/filepath"
)

// bounded_jsonl.go is the append+trim primitive shared by the miner's
// append-only operator files: the baseline mark (baseline_mark.go) and the
// capacity-decision ledger (action_ledger.go). Both are ports of v33 files
// with the same self-contained append/trim pair (provider/baseline.go
// baselineAppend/baselineTrim, provider/action_ledger.go
// ledgerAppend/ledgerTrim); keeping one copy in the miner means the two
// writers cannot drift on the crash-safety details that matter.

// appendBoundedLine appends one complete JSON line to path under the
// inter-process file lock, creating the parent directory, and then trims the
// file to the newest whole lines totalling at most maxBytes/2 when it has
// grown past maxBytes. Half the bound is the trim target so rotation does not
// happen on every append.
func appendBoundedLine(path string, line []byte, maxBytes int64) error {
	if len(line) == 0 || line[len(line)-1] != '\n' {
		line = append(line, '\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Blocking flock: released when the holder dies, so there is no stale-lock
	// case and a contended append waits rather than being dropped.
	release, err := acquireFileLock(path)
	if err != nil {
		return err
	}
	defer release()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if cerr != nil {
		return cerr
	}
	if st, err := os.Stat(path); err == nil && st.Size() > maxBytes {
		return trimJSONLToNewest(path, maxBytes/2)
	}
	return nil
}

// trimJSONLToNewest keeps the newest whole lines totalling at most keepBytes.
// A single newest line larger than the bound is kept rather than producing an
// empty file: over the bound for one entry beats losing the whole record. The
// caller holds the file lock.
func trimJSONLToNewest(path string, keepBytes int64) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	start := len(b)
	var size int64
	for i := len(b) - 1; i >= 0; {
		// find the start of the line ending at i
		j := i - 1
		for j >= 0 && b[j] != '\n' {
			j--
		}
		lineLen := int64(i - j)
		if size+lineLen > keepBytes {
			// j is the index of the newline BEFORE this line, so j+1 is where
			// this line starts; a zero size means this is the newest line and
			// it alone is over the bound, which is kept.
			if size == 0 {
				start = j + 1
			}
			break
		}
		size += lineLen
		start = j + 1
		i = j
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b[start:], 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
