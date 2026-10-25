package miner

import "github.com/urnetwork/connect"

// providerDisablesFamilyTransports reports whether this slot's provider should
// run one family-agnostic transport instead of the v4-pinned, v6-pinned and
// standby group.
//
// A proxied identity drops the group only once its proxy is KNOWN not to carry
// IPv6. Absent evidence keeps the group, because a proxy that does carry v6 can
// then still be classified dual-stack, and losing that is worse than one run of
// two idle transports. The verdict comes from the slot's own dial path
// (proxy_v6_observe.go), so it costs no probe and no extra goroutine.
//
// The direct identity: never. It dials on this host's own routes, so both
// pinned transports can honestly succeed there, and dropping them would cost
// the dual-stack record for a provider that has real IPv6.
//
// 3.23 gates this the same way in provider/family_transport.go:
// useFamilyTransports returns false for a proxied identity.
func providerDisablesFamilyTransports(proxySettings *connect.ProxySettings, verdict v6Verdict) bool {
	if proxySettings == nil {
		return false
	}
	return verdict == v6No
}
