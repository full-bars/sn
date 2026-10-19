package miner

import (
	"encoding/json"
	"time"
)

// baseline_mark.go ports the operator-mark half of the 3.23-fix v33 baseline
// recorder (provider/baseline.go: baselineMarkSample, baselineMark,
// baselineAppend). The sampling recorder itself (PM-06) is not in the miner,
// so a mark is the one line the miner writes: a timestamped annotation in
// <state>/baseline.jsonl that the unmodified `urnet-tools baseline
// show|compare` reads.
//
// The record shape is the v33 wire shape ({v, kind, label, ts}) and the
// operator path is the v33 one: the control socket is the only writer (a mark
// file written by a second process during a hot swap is exactly what the
// inter-process append lock exists for), an empty label is refused, and the
// reply carries the timestamp actually recorded so the operator can correlate
// their notes.

const (
	baselineFileName = "baseline.jsonl"
	// baselineMaxBytes caps the file. The v33 recorder allows a megabyte
	// (~600 bytes per sample); the mark path keeps the same bound so a flood
	// of marks cannot grow it without limit.
	baselineMaxBytes = 1 << 20
)

// baselineMarkRecord is one operator annotation: the v33 baselineSample
// reduced to the fields a mark carries. The sampler's measurement fields are
// deliberately absent - numbers on a mark would read as observations.
type baselineMarkRecord struct {
	V     int    `json:"v"`
	Kind  string `json:"kind"`
	Label string `json:"label,omitempty"`
	TS    string `json:"ts"`
}

// baselinePath returns <state>/baseline.jsonl, the file the baseline
// subcommands read.
func baselinePath() (string, error) {
	return providerStatePath(baselineFileName)
}

// baselineMark appends one operator mark and returns the timestamp written.
func baselineMark(label string) (time.Time, error) {
	now := time.Now()
	path, err := baselinePath()
	if err != nil {
		return time.Time{}, err
	}
	line, err := json.Marshal(baselineMarkRecord{
		V:     1,
		Kind:  "mark",
		Label: label,
		TS:    now.UTC().Format(time.RFC3339),
	})
	if err != nil {
		return time.Time{}, err
	}
	if err := appendBoundedLine(path, line, baselineMaxBytes); err != nil {
		return time.Time{}, err
	}
	return now, nil
}
