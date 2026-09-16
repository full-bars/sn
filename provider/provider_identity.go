package provider

import (
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/urnetwork/connect"
)

// writeProviderClientKeySeed persists the Ed25519 seed to
// `~/.urnetwork/.provider.key` with 0600 permissions (sensitive
// material — anyone with this file can impersonate the provider
// against the platform identity layer).
func writeProviderClientKeySeed(seed []byte) error {
	p, err := providerStatePath(".provider.key")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	return atomicWriteFile(p, seed, 0600)
}

// enableProviderEncryption turns on the per-peer e2e encryption sessions
// (post-quantum key exchange) on a provider's serving client. The provider
// serves plaintext and encrypted peers seamlessly: a session only forms when
// an initiator starts the handshake, and every enabled provider increases
// the set of peers that post-quantum initiators can reach. Opportunistic
// (not Required) so older consumers that cannot establish a session are
// still served.
func enableProviderEncryption(clientSettings *connect.ClientSettings) {
	if clientSettings.EncryptionSettings == nil {
		clientSettings.EncryptionSettings = connect.DefaultEncryptionSettings()
	}
	clientSettings.EncryptionSettings.Mode = connect.EncryptionModeOpportunistic
}

// readProviderTlsCertAndKey loads the sequence-level TLS server cert
// chain and matching private key from `~/.urnetwork/.provider.cert`
// (PEM, leaf first, possibly chained) and the private key from the
// same file (the PEM blocks are concatenated: cert blocks first,
// then a single `PRIVATE KEY` block). Returns (nil, nil, nil) when
// the file does not exist.
func readProviderTlsCertAndKey() (certPem []byte, keyPem []byte, returnErr error) {
	p, err := providerStatePath(".provider.cert")
	if err != nil {
		return nil, nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	// Split into cert blocks and the private key block.
	rest := b
	for {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		blockPem := pem.EncodeToMemory(block)
		if block.Type == "CERTIFICATE" {
			certPem = append(certPem, blockPem...)
		} else {
			keyPem = blockPem
			break
		}
		rest = next
	}
	return certPem, keyPem, nil
}

// writeProviderTlsCertAndKey persists the sequence-level TLS server
// cert and private key to `~/.urnetwork/.provider.cert` with 0600
// permissions. The cert blocks are written first, then the private
// key block, so the on-disk file is a self-contained PEM bundle.
func writeProviderTlsCertAndKey(certPem, keyPem []byte) error {
	p, err := providerStatePath(".provider.cert")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	out := make([]byte, 0, len(certPem)+len(keyPem))
	out = append(out, certPem...)
	out = append(out, keyPem...)
	return atomicWriteFile(p, out, 0600)
}

// proxy retry constants for URL-sourced proxies.
const (
	proxyURLGiveUpRetryBase = 15 * time.Minute
	proxyURLGiveUpRetryCap  = 24 * time.Hour

	// proxyURLGiveUpEvictAfterCycles is the lifetime give-up count at which a
	// URL-sourced address is permanently evicted (see evictProxyURLAddress)
	// instead of requeued again. At 4 cycles the doubling backoff reaches 2h
	// (15min → 30min → 1h → 2h), so a dead proxy is evicted after roughly
	// 4 hours of wall time. The 24h blacklist cooldown then prevents re-entry;
	// the blacklist pruner removes it after 24h, and the next URL fetch cycle
	// re-probes it from scratch (must pass the dual-stage probe to re-enter).
	proxyURLGiveUpEvictAfterCycles = 4
)
