package miner

// proxy_manager_audit_park.go is proxy audit's act half: the seam that turns a
// park decision into a stopped proxy slot, and a release into a proxy that can
// run again.
//
// v33 gives the audit a `proxyCancelMap map[string]context.CancelFunc` that its
// provide loop fills as it starts each proxy goroutine, and the park is a
// lookup-and-cancel in that map. The miner keeps the same per-slot cancels, but
// inside the live reloader (proxy_reloader.go), which owns the running set and
// has to see a cancel to keep its bookkeeping, its address claims and its
// published running set truthful. So the audit parks through the reloader
// instead of through a map of its own: the reloader's cancelSlots IS the
// cancellation, and a park for a proxy this run is not serving cancels nothing
// and is not recorded.
//
// The miner also needs what the provider does not: the desired set is applied
// on every reload trigger, and it still contains the identity the audit parked
// (a park rests a proxy, it does not edit the operator's proxy file). The hold
// the degraded reaper already uses for exactly this
// (proxy_manager_degraded_reaper.go) therefore records the park window too, and
// `filterReapHeldProxySettings` — applied wherever the reloader computes what it
// should be serving — keeps the identity out until the window ends. Releasing a
// park drops that hold early and re-applies the desired set once, so the proxy
// comes back on the tick that released it instead of on the next unrelated
// trigger.
//
// A park that is never explicitly released needs no special case: its hold and
// its backoff end at the same instant, and the degraded reaper's cycle releases
// elapsed holds and re-applies the desired set (releaseExpiredReapedProxyHolds),
// so the identity comes back on its own once the park window is over.

// liveProxyAuditPark cancels the named identities through the live reloader.
//
// A process with no reloader (no provide run yet, a profile run that pins its
// slot set, or a yielded run) parks nothing: there is no slot to stop, and
// claiming a park would mark a proxy parked in proxy.state while it kept
// serving. The reloader is resolved per call because the manager starts its
// loops before the provide run builds one.
func liveProxyAuditPark(keys []string, stillEligible func(addr string) bool, onParked func(addr string)) []string {
	reloader := currentMinerProxyReloader.Load()
	if reloader == nil {
		return nil
	}
	return reloader.cancelSlots(keys, stillEligible, onParked)
}

// liveProxyAuditUnpark drops the park holds for the named identities and
// re-applies the desired set so they can return.
//
// The reload is the point: the hold's only job is to keep the identity out of
// the desired set, so dropping it and waiting for the next unrelated trigger
// would leave a released proxy idle for as long as nothing else changed. A
// failed reload is logged and retried by the reload watcher on its next trigger;
// the holds are already gone, so nothing keeps the identity out.
func liveProxyAuditUnpark(keys []string) {
	released := make([]string, 0, len(keys))
	for _, key := range keys {
		if key == directProxyKey {
			continue
		}
		if releaseReapedProxyHold(key) {
			released = append(released, key)
		}
	}
	if len(released) == 0 {
		return
	}
	reloader := currentMinerProxyReloader.Load()
	if reloader == nil {
		return
	}
	if _, err := reloader.reload(); err != nil {
		tlog("[proxy][audit] warn: released %s but could not re-apply the desired set: %v\n",
			plural(len(released), "parked proxy", "parked proxies"), err)
		return
	}
	tlog("[proxy][audit] %s rejoined the pool\n", plural(len(released), "released proxy", "released proxies"))
}
