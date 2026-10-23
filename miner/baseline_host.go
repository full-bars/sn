package miner

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// baseline_host.go is the host-level half of the baseline recorder
// (provider/baseline.go readBaselineHost/buildBaselineHost). Every reader is
// best-effort and independent: a box with no cgroup limit still records host
// memory, and a box with an unreadable /proc/meminfo still records PSI and
// load. A reading that could not be taken is OMITTED, never written as zero.

// errNoHostData marks a host reader that could not produce a value. Callers
// omit the field rather than writing a zero.
var errNoHostData = errors.New("baseline: no host data")

// baselineHostRaw is the raw result of every host reader, before any omission
// decision. Each field is nil when its reader failed. Keeping the readings
// separate from the decision is what makes the omission rule testable without
// a real /proc, and it stops one failed reader from discarding the others.
type baselineHostRaw struct {
	HostMiB    int64 // -1 when unreadable
	CgroupMiB  int64 // -1 when there is no cgroup limit
	SwapUsed   *int64
	PSISome60  *float64
	PSIFull60  *float64
	PSIFull300 *float64
	Load1      *float64
}

// readBaselineHost gathers the host-level readings.
func readBaselineHost() (baselineHost, error) {
	raw := baselineHostRaw{
		HostMiB:   readMemAvailableMiB(),
		CgroupMiB: readCgroupAvailableMiB(),
	}
	if used, ok := readSwapUsedMiB(); ok {
		raw.SwapUsed = &used
	}
	// The resource file is /proc/pressure/memory; "some" is the line inside it
	// that readPSI parses. Passing the line name here read a file that never
	// exists, so the reading was silently omitted on every node.
	if some, err := readPSI("memory"); err == nil {
		raw.PSISome60 = &some
	}
	if full60, full300, err := readPSIFull(); err == nil {
		raw.PSIFull60, raw.PSIFull300 = &full60, &full300
	}
	if l1, _, err := getSystemLoad(); err == nil {
		raw.Load1 = &l1
	}
	return buildBaselineHost(raw)
}

// buildBaselineHost applies the omission rules to the raw readings. Per brief
// #2074 mem_available_mib is the TIGHTER of host and cgroup, with avail_source
// naming which supplied it and host winning a tie. When neither memory reader
// worked, the memory fields are omitted but everything else still stands.
//
// The comparisons are >= 0: a real reading of 0 means memory is exhausted, and
// treating that as no-data would hide the case that matters most. Both readers
// return -1 on error, so 0 is real data.
func buildBaselineHost(raw baselineHostRaw) (baselineHost, error) {
	h := baselineHost{
		SwapUsedMiB:   raw.SwapUsed,
		PSISomeAvg60:  raw.PSISome60,
		PSIFullAvg60:  raw.PSIFull60,
		PSIFullAvg300: raw.PSIFull300,
		Load1:         raw.Load1,
	}
	host, cgroup := raw.HostMiB, raw.CgroupMiB
	switch {
	case host >= 0 && cgroup >= 0:
		v, src := host, "host"
		if cgroup < host {
			v, src = cgroup, "cgroup"
		}
		h.MemAvailableMiB, h.AvailSource = &v, src
	case host >= 0:
		h.MemAvailableMiB, h.AvailSource = &host, "host"
	case cgroup >= 0:
		h.MemAvailableMiB, h.AvailSource = &cgroup, "cgroup"
	default:
		// No memory reading at all: omit those two fields, keep the rest.
		return h, errNoHostData
	}
	return h, nil
}

// readSwapUsedMiB reads /proc/meminfo: SwapTotal minus SwapFree, omitted when
// either is unreadable.
func readSwapUsedMiB() (int64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	var total, free int64
	var haveTotal, haveFree bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		var dst *int64
		var have *bool
		switch {
		case strings.HasPrefix(line, "SwapTotal:"):
			dst, have = &total, &haveTotal
		case strings.HasPrefix(line, "SwapFree:"):
			dst, have = &free, &haveFree
		default:
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// /proc/meminfo reports kB.
		if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			*dst = v / 1024
			*have = true
		}
	}
	if !haveTotal || !haveFree {
		return 0, false
	}
	return total - free, true
}

// readPSIFull returns the memory "full" line's avg60 and avg300. The "full" line
// lives in /proc/pressure/memory; there is no /proc/pressure/full. readPSI only
// parses "some", and the baseline needs both the full-pressure stall time and
// its longer window, so this is a separate reader.
func readPSIFull() (avg60, avg300 float64, err error) {
	b, err := os.ReadFile("/proc/pressure/memory")
	if err != nil {
		return 0, 0, err
	}
	return parsePSIFull(string(b))
}

// parsePSIFull parses the "full" line. Mirrors parsePSISome, which covers only
// "some" and only avg60.
func parsePSIFull(content string) (avg60, avg300 float64, err error) {
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "full ") {
			continue
		}
		for _, field := range strings.Fields(line)[1:] {
			k, v, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch k {
			case "avg60":
				avg60, err = strconv.ParseFloat(v, 64)
			case "avg300":
				avg300, err = strconv.ParseFloat(v, 64)
			}
			if err != nil {
				return 0, 0, err
			}
		}
		return avg60, avg300, nil
	}
	return 0, 0, fmt.Errorf("no 'full' line in PSI content")
}
