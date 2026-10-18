package miner

// control_audit.go adapts the P2 control socket to the P1(c) audit ring
// (proxy_manager_audit_ring.go). The daemon branch originally carried its own
// reduced ring; after the rebase both existed. The socket now uses the full
// v33 port (checksum envelope, hotswap persistence, union/dedupe) and this
// file only keeps the call-site names the run path and the socket already use.

// initControlAuditRing ensures the process-wide audit ring exists. The run
// path calls it beside the control socket, before any command is served.
func initControlAuditRing() {
	if globalAuditRing == nil {
		initAuditRing()
	}
}

// recordControlProcessStart appends this process's startup to the ring so
// `urnet-tools history` shows lifecycle events across restarts.
func recordControlProcessStart() {
	recordProcessStart(false, false)
}

// recordControlAudit appends one control-socket operation and persists on the
// shared 30s cadence (proxy_manager_audit_ring.go).
func recordControlAudit(entry CommandAudit) {
	recordAndPersist(entry)
}

// forceControlAuditPersist persists immediately (shutdown and handoff paths).
func forceControlAuditPersist() {
	forceAuditPersist()
}
