package connectx

import (
	"strings"

	"golang.org/x/net/proxy"
)

// proxyKeyUserSep separates the address from the user in a proxy key. It is the
// ASCII unit separator, which cannot appear in a hostname, port or URL-encoded
// user, so a key can be split back into its parts unambiguously.
const proxyKeyUserSep = "\x1f"

// SplitProxyKey splits a proxy key into its address and its user. A key with no
// separator yields the whole key as the address and an empty user.
//
// Carried from 3.23-fix's net.go, which cannot be copied whole because it also
// needs the engine's Logger. Only this function and its separator are portable,
// and proxyKeyDisplay in the provider package is what consumes them.
func SplitProxyKey(key string) (address, user string) {
	address, user, ok := strings.Cut(key, proxyKeyUserSep)
	if !ok {
		return key, ""
	}
	return address, user
}

// ProxyKey builds the stable identity for a proxy from its address and
// authenticated user, and is the inverse of SplitProxyKey.
//
// The password is deliberately excluded: a credential rotation (same account,
// new password) must stay the same identity so its ID, health history and
// earnings survive the rotation. Two proxies that differ only by password are a
// rotation, not a new proxy; two proxies at the same address with different
// users are genuinely different proxies.
//
// 3.23-fix has this as a method on its own ProxySettings type. Upstream's
// ProxySettings has no Key method and its type cannot be extended from
// outside the package, so it is provided here as a function instead; the
// provider package calls connectx.ProxyKey(p.Address, p.Auth).
func ProxyKey(address string, auth *proxy.Auth) string {
	if auth == nil || auth.User == "" {
		return address
	}
	return address + proxyKeyUserSep + auth.User
}
