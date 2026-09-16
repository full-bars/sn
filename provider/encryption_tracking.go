package provider

import (
	"sync"

	"github.com/urnetwork/connect"
)

// encryptionManagers is a thread-safe registry of the live per-proxy encryption
// session managers. Multiple providers (a native client plus each proxy's
// client) each own their own connect client -> encryption manager, so a single
// pointer was both a data race (written per provideWithProxy, read by the
// periodic [pqe] line) and dropped every manager but the last. The [pqe] line
// sums counts across every registered manager.
var encryptionManagers = struct {
	mu  sync.Mutex
	set []*connect.EncryptionSessionManager
}{}

func registerEncryptionManager(m *connect.EncryptionSessionManager) {
	if m == nil {
		return
	}
	encryptionManagers.mu.Lock()
	defer encryptionManagers.mu.Unlock()
	encryptionManagers.set = append(encryptionManagers.set, m)
}

func unregisterEncryptionManager(m *connect.EncryptionSessionManager) {
	if m == nil {
		return
	}
	encryptionManagers.mu.Lock()
	defer encryptionManagers.mu.Unlock()
	for i, x := range encryptionManagers.set {
		if x == m {
			encryptionManagers.set = append(encryptionManagers.set[:i], encryptionManagers.set[i+1:]...)
			return
		}
	}
}
