//go:build !linux

package miner

// collectMinerPlatformResources leaves unsupported readings absent.
func collectMinerPlatformResources(_ *snapshotResources) {}

// controlReportOpenFDs is unavailable off Linux; the snapshot omits both
// fields rather than guessing.
func controlReportOpenFDs() (open int, limit int, ok bool) {
	return 0, 0, false
}
