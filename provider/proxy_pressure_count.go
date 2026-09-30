package provider

// runningProxyCountForPressure is the pool size the goroutine sensor and the
// control socket report, excluding the direct transport. It lives outside
// resource_pressure.go because that file is linux-only, while the health
// registry it reads is not: a non-linux build still has a running proxy count
// to report, and stubbing it to zero would claim the pool is empty.
// runningProxyCountForPressure is the pool size the goroutine sensor divides
// by: the health registry count minus the native direct transport, which is a
// single fixed goroutine, not a pool member. The direct entry is registered at
// index 0 by default, so WITHOUT this the sensor read 1 running proxy on a
// direct-only node, divided by 1, and judged a ~1,000-goroutine background
// (which is roughly the process's fixed overhead) as an emergency blowout.
// With it excluded, a direct-only node has RunningProxies == 0 and falls back
// to the absolute ramp, as the sensor documents.
func runningProxyCountForPressure() int {
	n := ProxyHealthCount()
	if ProxyKeyByIndex(0) == directProxyKey {
		n--
	}
	return n
}
