//go:build !linux

package miner

import "errors"

// baseline_host_other.go is the non-Linux stub for the host readers. Every
// reader reports no data, so a sample omits the host fields rather than writing
// zeros that would read as measurements.

var errNoHostReadings = errors.New("baseline: no host readings")

func readPSI(_ string) (avg60 float64, err error) { return 0, errNoHostReadings }

func readMemAvailableMiB() int64 { return -1 }

func readCgroupAvailableMiB() int64 { return -1 }

func getSystemLoad() (load1, load5 float64, err error) { return 0, 0, errNoHostReadings }
