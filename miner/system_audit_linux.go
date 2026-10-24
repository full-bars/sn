//go:build linux

package miner

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

// system_audit_linux.go is the reader half of the host audit
// (system_audit_linux.go's counterpart in the provider, provider/
// system_audit_linux.go; the grading and the reporting are in
// system_audit.go). Everything here is a read: one getrlimit and three files
// under /proc.

// hostAuditProcFS is where the kernel publishes these knobs.
const hostAuditProcFS = "/proc"

// hostAuditProcRoot is hostAuditProcFS, a variable so tests can point the
// readers at a fixture tree instead of the test host's real limits.
var hostAuditProcRoot = hostAuditProcFS

// hostAuditProcFiles are the procfs entries the audit reads, named relative to
// hostAuditProcRoot. The values are the kernel's layout, not this file's
// invention, and the platform test checks them against absolute paths written
// down independently. The set is a variable only so that test can prove the
// fixtures would catch a wrong one (the missing "sys/" segment this port
// shipped once); nothing at runtime reassigns it.
var hostAuditProcFiles = struct {
	ConntrackMax     string
	ConntrackTimeout string
	Meminfo          string
}{
	ConntrackMax:     "sys/net/netfilter/nf_conntrack_max",
	ConntrackTimeout: "sys/net/netfilter/nf_conntrack_tcp_timeout_established",
	Meminfo:          "meminfo",
}

// hostAuditLiveFileLimit reads the process's soft RLIMIT_NOFILE from the
// kernel. It is a named function, not just a closure in the variable below, so
// a test can exercise the real syscall while the seam is installed for the
// other tests.
func hostAuditLiveFileLimit() *uint64 {
	var rlimit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &rlimit); err != nil {
		return nil
	}
	limit := rlimit.Cur
	return &limit
}

// hostAuditFileLimit is the reader the audit uses. A variable so a test can
// supply a limit without changing the limit of the process it runs in, which
// would affect the rest of the suite.
var hostAuditFileLimit = hostAuditLiveFileLimit

// gatherHostAuditObservations reads what the kernel will tell us about the
// three limits. Any value it cannot read stays nil and is reported as
// unobservable rather than as zero.
func gatherHostAuditObservations() hostAuditObservations {
	obs := hostAuditObservations{
		FdLimit: hostAuditFileLimit(),
	}
	if max, ok := readProcInt(filepath.Join(hostAuditProcRoot, hostAuditProcFiles.ConntrackMax)); ok {
		obs.ConntrackMax = &max
	}
	if timeout, ok := readProcInt(filepath.Join(hostAuditProcRoot, hostAuditProcFiles.ConntrackTimeout)); ok {
		obs.ConntrackTimeoutEstablished = &timeout
	}
	// The conntrack table is sized against physical RAM by the command the
	// hint names, so the target uses MemTotal, not the cgroup-limited memory
	// the provider plan budgets against: a container's conntrack table is
	// sized by the RAM the sysctl was written for, and optimize reads the same
	// MemTotal it finds in its namespace.
	if ramBytes := parseMeminfoTotal(readSmallFile(filepath.Join(hostAuditProcRoot, hostAuditProcFiles.Meminfo))); ramBytes > 0 {
		target := hostAuditConntrackMaxForRAM(int64(ramBytes / 1024))
		obs.ConntrackMaxTarget = &target
	}
	return obs
}

// hostAuditConntrackMaxForRAM mirrors internal/urnettools/legacy_cmds.go
// conntrackMaxForRAMKB: an entry costs roughly 300 bytes and the table gets
// about 5% of RAM. It is duplicated rather than imported because
// internal/urnettools is the operator CLI and pulls in cobra and every
// command; the miner should not link that to read one switch.
//
// The duplication is load-bearing: the hint tells the operator to run
// `urnet-tools optimize`, so this table has to stay the table optimize writes.
// If the brackets change there, change them here.
func hostAuditConntrackMaxForRAM(ramKB int64) int64 {
	switch {
	case ramKB < 1*1024*1024: // <1GB
		return 131072 // 128K entries ~37MB
	case ramKB < 4*1024*1024: // 1-4GB
		return 262144 // 256K entries ~75MB
	case ramKB < 8*1024*1024: // 4-8GB
		return 524288 // 512K entries ~150MB
	case ramKB < 16*1024*1024: // 8-16GB
		return 1048576 // 1M entries ~300MB
	case ramKB < 32*1024*1024: // 16-32GB
		return 2097152 // 2M entries ~600MB
	default: // >32GB
		return 4194304 // 4M entries ~1.2GB
	}
}
