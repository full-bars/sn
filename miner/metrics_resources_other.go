//go:build !linux

package miner

// readProcRSS is unavailable off Linux; the gauge is omitted rather than
// guessing, so a missing urnet_rss_bytes series means unknown.
func readProcRSS() (uint64, bool) {
	return 0, false
}
