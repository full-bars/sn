//go:build !linux

package miner

// system_audit_other.go is the non-Linux half of the host audit. The checks
// read /proc and RLIMIT_NOFILE, and the provider's own non-Linux audit is a
// no-op (provider/system_audit_other.go: RunSystemAudit returns false, false),
// so startup stays silent here rather than warning about a limit it cannot
// read. The disk half still runs where it is supported.
func gatherHostAuditObservations() hostAuditObservations {
	return hostAuditObservations{}
}
