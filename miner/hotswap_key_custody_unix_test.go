//go:build linux || darwin

package miner

import (
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/urfoundation/sn/clientauth"
)

// The failure seen on a live host: the candidate opened the provider key after
// TAKEOVER, met the parent's owner lock and exited with startup_recovery_required.
// This runs the same two opens against the real key owner.
func TestHandoffBatonLetsTheCandidateOpenTheProviderKey(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", shortStateDir(t))
	closeProviderStore()
	t.Cleanup(closeProviderStore)

	keyPath := filepath.Join(t.TempDir(), ".provider.key")
	parentKey, err := clientauth.OpenProviderClientKey(t.Context(), keyPath, clientauth.ProviderClientKeyOptions{AllowCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parentKey.Close() })
	setHotSwapKeyCustody(parentKey)
	t.Cleanup(func() { setHotSwapKeyCustody(nil) })

	parentConn, candidateConn := net.Pipe()
	t.Cleanup(func() { _ = candidateConn.Close() })
	session := &HotswapParentSession{Reader: bufio.NewReader(parentConn), Writer: parentConn}

	opened := make(chan error, 1)
	go func() {
		if msg, err := readHotswapMessage(bufio.NewReader(candidateConn)); err != nil || msg.Type != HotswapMsgTakeover {
			opened <- errors.New("candidate did not receive TAKEOVER")
			return
		}
		candidateKey, openErr := clientauth.OpenProviderClientKey(t.Context(), keyPath, clientauth.ProviderClientKeyOptions{})
		if candidateKey != nil {
			_ = candidateKey.Close()
		}
		_, _ = io.WriteString(candidateConn, `{"type":"ACK"}`+"\n")
		opened <- openErr
	}()

	if err := handoffBatonSendTakeover(session, os.Getpid(), 4304); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if openErr := <-opened; openErr != nil {
		t.Fatalf("the candidate could not open the key the parent released: %v", openErr)
	}
}
