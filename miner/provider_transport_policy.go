package miner

import "github.com/urnetwork/connect"

// providerDisablesFamilyTransports reports whether this slot's provider should
// run one family-agnostic transport instead of the v4-pinned, v6-pinned and
// standby group.
//
// A proxied identity: yes. The pinned transports dial from this host, while the
// slot's own dial goes through a SOCKS proxy, so a pinned v6 transport has no
// route to the platform and can only fail repeatedly. The family-agnostic
// transport hands the proxy a hostname, so a proxy with v6 egress can still
// reach the platform over v6.
//
// The direct identity: no. It dials on this host's own routes, so both pinned
// transports can honestly succeed there, and dropping them would cost the
// dual-stack record for a provider that has real IPv6.
//
// 3.23 gates this the same way in provider/family_transport.go:
// useFamilyTransports returns false for a proxied identity.
func providerDisablesFamilyTransports(proxySettings *connect.ProxySettings) bool {
	return proxySettings != nil
}
