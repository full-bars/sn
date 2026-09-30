package provider

import (
	"reflect"
	"testing"
)

func TestResolveAuditReleaseKeys(t *testing.T) {
	u1, u2 := identityKey("gw.example:1080", "u1"), identityKey("gw.example:1080", "u2")
	other := identityKey("other.example:1080", "u1")
	known := []string{u2, other, u1, "10.0.0.5:1080"}

	// An operator can only type an address: it names every identity there.
	if got, want := resolveAuditReleaseKeys("gw.example:1080", known), []string{u1, u2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bare address must resolve to every identity at it: got %q want %q", got, want)
	}
	// An exact identity key names only itself.
	if got, want := resolveAuditReleaseKeys(u1, known), []string{u1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("exact key must resolve to itself only: got %q want %q", got, want)
	}
	// An unauthenticated proxy's key IS its address.
	if got, want := resolveAuditReleaseKeys("10.0.0.5:1080", known), []string{"10.0.0.5:1080"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bare unauthenticated key: got %q want %q", got, want)
	}
	// Nothing known: keep the input so a stray backoff/failure count is still cleared.
	if got, want := resolveAuditReleaseKeys("9.9.9.9:1", known), []string{"9.9.9.9:1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unknown address must fall through: got %q want %q", got, want)
	}
}
