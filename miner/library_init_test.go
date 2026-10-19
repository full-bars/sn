package miner

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain captures after every dependency and miner init has finished,
// before testing.M.Run intentionally aliases stderr in test2json mode.
var minerLibraryStreamsIndependent bool

// All library initialization has completed here. A package variable's own
// initializer would run too early to observe side effects of miner init.
func TestMain(m *testing.M) {
	minerLibraryStreamsIndependent = os.Stdout != os.Stderr && os.Stdout.Fd() != os.Stderr.Fd()

	// Keep the package's tests off the machine's real client JWT store.
	// Hot restart is default-on, so a warm <state>/.client_jwts.json would
	// otherwise hand the provider fixtures a stored identity instead of the
	// mint/registration path they assert. Tests that need a specific store
	// swap this one (withGlobalStore).
	storeDir, err := os.MkdirTemp("", "miner-jwt-store-test-")
	if err != nil {
		panic(err)
	}
	originalStore := loadGlobalClientJWTStore()
	storeGlobalClientJWTStore(newClientJWTStore(filepath.Join(storeDir, ".client_jwts.json")))

	code := m.Run()

	storeGlobalClientJWTStore(originalStore)
	_ = os.RemoveAll(storeDir)
	os.Exit(code)
}

// The observed boundary is library initialization, not the later test runner.
func TestLibraryInitializationKeepsStdoutAndStderrIndependent(t *testing.T) {
	if !minerLibraryStreamsIndependent {
		t.Fatal("importing the miner library aliased stderr to stdout")
	}
}
