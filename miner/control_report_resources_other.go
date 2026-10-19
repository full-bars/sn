//go:build !linux

package miner

// controlReportOpenFDs is unavailable off Linux; the snapshot omits both
// fields rather than guessing.
func controlReportOpenFDs() (open int, limit int, ok bool) {
	return 0, 0, false
}
