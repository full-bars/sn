package miner

// host_capability.go records what this host can actually reach, so the
// activation budget charges a pass for the posts it will really spend.
//
// A box without usable IPv6 posts once per pass, not twice. Charging it two
// posts anyway halves the passes it is allowed in the operator's six-per-hour
// window for no reason, so the capability is measured and stored rather than
// assumed.

import (
	"context"
	"net"
	"time"
)

const (
	// providerStoreKeyHostCapability is the store record this file owns.
	providerStoreKeyHostCapability = "host_capability"

	// A 1-byte UDP send is the cheapest test that exercises the kernel's route
	// decision. net.Dial succeeds on a blackholed v6 route, which is why the
	// Android egress probe was changed to send rather than dial.
	ipv6ProbeTarget  = "[2001:4860:4860::8888]:53"
	ipv6ProbeTimeout = 1500 * time.Millisecond

	// Cadence. A host that has had IPv6 recently is probably flapping or
	// waiting for a tunnel, and restoring it doubles what a pass earns, so it
	// is re-checked often. A host that has never had it is re-checked rarely;
	// an address change triggers an immediate check anyway.
	capabilityRecheckHadIPv6Recently = 15 * time.Minute
	capabilityRecheckHadIPv6LongAgo  = time.Hour
	capabilityRecheckNeverHadIPv6    = 6 * time.Hour
	capabilityRecentWindow           = 24 * time.Hour

	// interfaceFingerprintInterval is how often the address list is compared
	// without sending anything. A change means a network change, which is the
	// moment to spend a probe.
	interfaceFingerprintInterval = 3 * time.Minute
)

// hostCapability is the stored measurement.
type hostCapability struct {
	// IPv6Usable is the last measurement.
	IPv6Usable bool `json:"ipv6_usable"`
	// EverUsable records that this host has had usable IPv6 at some point,
	// which separates "flapping" from "never had it" for the recheck cadence.
	EverUsable bool `json:"ever_usable,omitempty"`
	// CheckedAtUnix is when the measurement was taken.
	CheckedAtUnix int64 `json:"checked_at_unix,omitempty"`
	// LastIPv6UsableAtUnix is the last time it measured usable.
	LastIPv6UsableAtUnix int64 `json:"last_ipv6_usable_at_unix,omitempty"`
	// LastError keeps the reason a probe failed, for the status line.
	LastError string `json:"last_error,omitempty"`
}

func readHostCapability() hostCapability {
	var cap hostCapability
	if store := providerStoreInstance(); store != nil {
		store.load(providerStoreBucketState, providerStoreKeyHostCapability, &cap)
	}
	return cap
}

func writeHostCapability(cap hostCapability) {
	if store := providerStoreInstance(); store != nil {
		_ = store.save(providerStoreBucketState, providerStoreKeyHostCapability, cap)
	}
}

// probeIPv6Usable sends one byte over IPv6 and reports whether the kernel
// accepted it. A host with no global v6 address is rejected without a syscall.
func probeIPv6Usable() (bool, string) {
	if !hasGlobalIPv6Address() {
		return false, "no global ipv6 address"
	}
	conn, err := net.DialTimeout("udp6", ipv6ProbeTarget, ipv6ProbeTimeout)
	if err != nil {
		return false, err.Error()
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(ipv6ProbeTimeout))
	if _, err := conn.Write([]byte{0}); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// hasGlobalIPv6Address reports whether any interface carries a routable v6
// address. Loopback and link-local do not count: neither can reach the
// platform.
func hasGlobalIPv6Address() bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.To4() != nil {
			continue
		}
		ip := ipnet.IP
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			continue
		}
		return true
	}
	return false
}

// capabilityRecheckInterval is how long to wait before measuring again, given
// what the record says about this host's history.
func capabilityRecheckInterval(cap hostCapability, now time.Time) time.Duration {
	if cap.IPv6Usable {
		return capabilityRecheckHadIPv6Recently
	}
	if !cap.EverUsable {
		return capabilityRecheckNeverHadIPv6
	}
	if cap.LastIPv6UsableAtUnix > 0 && now.Sub(time.Unix(cap.LastIPv6UsableAtUnix, 0)) <= capabilityRecentWindow {
		return capabilityRecheckHadIPv6Recently
	}
	return capabilityRecheckHadIPv6LongAgo
}

// measureHostCapability probes once and stores the result.
func measureHostCapability(now time.Time) hostCapability {
	usable, probeErr := probeIPv6Usable()
	cap := readHostCapability()
	cap.IPv6Usable = usable
	cap.CheckedAtUnix = now.Unix()
	cap.LastError = probeErr
	if usable {
		cap.EverUsable = true
		cap.LastIPv6UsableAtUnix = now.Unix()
	}
	writeHostCapability(cap)
	return cap
}

// watchHostCapability measures at startup, then re-measures on the cadence the
// record implies, and immediately when the interface address list changes.
// It never blocks startup and stops when the context ends.
func watchHostCapability(ctx context.Context) {
	now := time.Now()
	cap := readHostCapability()
	// Startup always measures: the previous value may be days old, and the
	// first pass should charge the right number of posts.
	cap = measureHostCapability(now)
	fingerprint := interfaceFingerprint()

	next := now.Add(capabilityRecheckInterval(cap, now))
	ticker := time.NewTicker(interfaceFingerprintInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-ticker.C:
			changed := false
			if f := interfaceFingerprint(); f != fingerprint {
				fingerprint = f
				changed = true
			}
			if !changed && at.Before(next) {
				continue
			}
			cap = measureHostCapability(at)
			next = at.Add(capabilityRecheckInterval(cap, at))
		}
	}
}

// interfaceFingerprint is a cheap digest of the interface address list. It
// costs no packets and changes when the host's addressing changes.
func interfaceFingerprint() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	b := make([]byte, 0, len(addrs)*16)
	for _, a := range addrs {
		b = append(b, a.String()...)
		b = append(b, '\n')
	}
	return string(b)
}

// hostActivationPosts is how many posts one activation pass spends here: one
// per address family the host can actually reach. An unmeasured host is
// charged two, the conservative side, so a missing record never overspends.
func hostActivationPosts() int {
	cap := readHostCapability()
	if cap.IPv6Usable {
		return 2
	}
	if cap.CheckedAtUnix == 0 {
		return 2
	}
	return 1
}
