//go:build !linux && !darwin

package miner

import (
	"github.com/urnetwork/connect"
)

// hostMemoryByteCount is unknown on this platform. Zero keeps the unknown-host
// 64 MiB per-provider target (providerUnknownHostDeviceMemoryTargetByteCount),
// since there is no host memory to derive from, and bounds the composed soft
// limit with providerUnknownHostAssumedHostByteCount -- announced once by
// newProviderMemoryPlan -- instead of letting 3 x count x 64 MiB grow above the
// memory the host actually has.
func hostMemoryByteCount() connect.ByteCount {
	return 0
}
