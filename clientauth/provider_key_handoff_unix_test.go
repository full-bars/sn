//go:build linux || darwin

package clientauth

import (
	"errors"
	"path/filepath"
	"testing"
)

// A hot swap candidate takes the key's owner lock without waiting, so the
// retiring process has to let go of it first and can take it back when the
// handoff aborts.
func TestProviderKeyOwnerLockHandsOverAndComesBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".provider.key")
	parent, err := OpenProviderClientKey(t.Context(), path, ProviderClientKeyOptions{AllowCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { parent.Close() })

	if _, err := OpenProviderClientKey(t.Context(), path, ProviderClientKeyOptions{}); !errors.Is(err, errRegistrationOwnerActive) {
		t.Fatalf("candidate open while the parent holds the lock = %v, want an active owner", err)
	}
	if err := parent.ReleaseForHotSwap(); err != nil {
		t.Fatal(err)
	}
	candidate, err := OpenProviderClientKey(t.Context(), path, ProviderClientKeyOptions{})
	if err != nil {
		t.Fatalf("candidate open after the parent released the lock: %v", err)
	}
	if err := parent.ReacquireAfterAbortedHotSwap(); !errors.Is(err, errRegistrationOwnerActive) {
		t.Fatalf("reacquire while the candidate holds the lock = %v, want an active owner", err)
	}
	if err := candidate.Close(); err != nil {
		t.Fatal(err)
	}
	if err := parent.ReacquireAfterAbortedHotSwap(); err != nil {
		t.Fatalf("reacquire after the candidate exited: %v", err)
	}
	if _, err := OpenProviderClientKey(t.Context(), path, ProviderClientKeyOptions{}); !errors.Is(err, errRegistrationOwnerActive) {
		t.Fatalf("open after the parent took the lock back = %v, want an active owner", err)
	}
}
