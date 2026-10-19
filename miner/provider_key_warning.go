// The accepted all-zero provider key must never pass silently. v33 used any
// nonempty raw seed (readProviderClientKeySeed plus the caller's 0 < len(seed)
// check); a live 3.23-fix provider can leave the all-zero seed, whose public
// key the server holds. The key gate accepts it so the automatic migration can
// preserve the existing identities, but the seed is publicly known: anyone can
// sign as this provider's client identity. The run warns on stderr — the same
// stream that carries the startup refusal — and the operator rotates
// deliberately afterwards.
package miner

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"io"
)

// providerZeroSeedWarning is the operator-facing line for an accepted all-zero
// provider key. It is written directly to stderr for the same reason the
// startup refusal is: the warning must survive a diagnostic sink the exporter
// had to refuse (a stdout redirected to a regular file cannot promise a
// bounded write).
const providerZeroSeedWarning = "WARNING: provider client key is the all-zero seed, a publicly known key: this provider's client identity can be impersonated by anyone. The retained key stays in use so the automatic import can preserve the existing identities; rotate deliberately afterwards with --allow-client-registration, which mints a fresh seed and keeps a timestamped backup of this file. See miner/PROVIDER-REGISTRATION.md."

// isAllZeroProviderSeed reports whether seed is exactly the 32-byte all-zero
// Ed25519 seed v33 could retain.
func isAllZeroProviderSeed(seed []byte) bool {
	return len(seed) == ed25519.SeedSize && bytes.Equal(seed, make([]byte, ed25519.SeedSize))
}

// warnZeroProviderSeed writes the zero-seed warning to w when the accepted
// retained key is the all-zero seed, and reports whether it wrote.
func warnZeroProviderSeed(w io.Writer, seed []byte) bool {
	if w == nil || !isAllZeroProviderSeed(seed) {
		return false
	}
	fmt.Fprintln(w, providerZeroSeedWarning)
	return true
}
