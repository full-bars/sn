package miner

import (
	"crypto/sha256"

	"github.com/urnetwork/connect"
)

// providerSlotIdentities resolves the per-slot identity of the configured
// proxies.
//
// connect.ProxySettings.Key() (connect net_proxy_key.go) is the 3.23-fix
// identity: the address alone, or address + user when the proxy has an auth
// user. Two proxies at the same host:port with different usernames are
// genuinely different identities with different client ids, and password is
// deliberately not part of the key.
//
// The sn slot hash historically covered Network + address only, so an address
// shared by two or more accounts collapsed onto one slot and the startup
// validation refused the whole run. This resolver keeps every single-use
// address byte-identical to that historical hash (sha256(Network + "\x00" +
// Address)) and, only for an address that appears two or more times in the
// configured list, hashes the full 3.23-fix identity instead
// (sha256(Network + "\x00" + Key())). Unique addresses therefore keep their
// credential file names and progress slots across upgrades, and shared
// addresses stay distinct exactly as 3.23-fix keys them, never merging two
// identities into one credential file.
type providerSlotIdentities struct {
	shared map[string]bool
}

func newProviderSlotIdentities(providers []*connect.ProxySettings) providerSlotIdentities {
	counts := map[string]int{}
	for _, settings := range providers {
		if settings == nil {
			continue
		}
		counts[settings.Network+"\x00"+settings.Address]++
	}
	shared := map[string]bool{}
	for key, count := range counts {
		if 1 < count {
			shared[key] = true
		}
	}
	return providerSlotIdentities{shared: shared}
}

// hash is the slot hash of one configured proxy. settings must be non-nil;
// callers keep their own direct (nil) handling.
func (self providerSlotIdentities) hash(settings *connect.ProxySettings) [32]byte {
	key := settings.Network + "\x00" + settings.Address
	if self.shared[key] {
		key = settings.Network + "\x00" + settings.Key()
	}
	return sha256.Sum256([]byte(key))
}
