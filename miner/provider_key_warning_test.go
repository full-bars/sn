// The accepted all-zero provider key warns loudly and only for that key.
package miner

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"
)

// The warning must name the exact risk (a publicly known, impersonatable
// client identity) and the deliberate rotation path, and it must stay silent
// for every other retained key shape.
func TestZeroProviderSeedWarningIsLoudAndScoped(t *testing.T) {
	zero := make([]byte, ed25519.SeedSize)
	var out bytes.Buffer
	if !warnZeroProviderSeed(&out, zero) {
		t.Fatal("the all-zero seed did not warn")
	}
	text := out.String()
	for _, want := range []string{"WARNING", "all-zero seed", "publicly known", "impersonated", "--allow-client-registration", "timestamped backup", "miner/PROVIDER-REGISTRATION.md"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the zero-seed warning lost %q: %s", want, text)
		}
	}
	normal := bytes.Repeat([]byte{0x42}, ed25519.SeedSize)
	out.Reset()
	if warnZeroProviderSeed(&out, normal) || out.Len() != 0 {
		t.Fatalf("a normal seed warned: %q", out.String())
	}
	if isAllZeroProviderSeed(normal) || !isAllZeroProviderSeed(zero) || isAllZeroProviderSeed(zero[:31]) || isAllZeroProviderSeed(nil) {
		t.Fatal("the zero-seed predicate is not exact")
	}
}
