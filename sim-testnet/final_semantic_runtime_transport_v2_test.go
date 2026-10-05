//go:build linux || darwin

// Historical reader facades preserve the real transport's replacement epoch
// across their exact metadata, storage and retained transcript boundaries.
package main

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"

	gsrpcclient "github.com/centrifuge/go-substrate-rpc-client/v4/client"
)

// Only the completed-call generation changes. The real local transport owns
// every encoded response, metadata decode and downstream admission decision.
type finalNativeTransportTestClientV2 struct {
	gsrpcclient.Client
	generation atomic.Uint64
	after      func(string, []any)
}

// Matches the upstream reconnect contract without replacing an RPC result.
func (self *finalNativeTransportTestClientV2) TransportGeneration() uint64 {
	return self.generation.Load()
}

// The synchronous response boundary is the explicit replacement barrier.
func (self *finalNativeTransportTestClientV2) CallContext(ctx context.Context, target any, method string, args ...any) error {
	err := self.Client.CallContext(ctx, target, method, args...)
	if err == nil && self.after != nil {
		self.after(method, args)
	}
	return err
}

// The late UID batch must repeat genesis, artifact and all reward vectors.
// Caller metadata and the original historical block remain independently owned.
func TestFinalNativeRewardV2ReconnectRepeatsCompleteHistoricalRead(t *testing.T) {
	f := newFinalNativeRewardHistoryFixtureV2(t)
	baseline, err := readFinalNativeRewardAtV2(t.Context(), f.native, f.head, 521, f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	client := &finalNativeTransportTestClientV2{Client: f.native.API.Client}
	client.generation.Store(1)
	f.native.API.Client = client
	genesisReads, batches := 0, 0
	client.after = func(method string, args []any) {
		if method == "chain_getBlockHash" && args[0] == uint64(0) {
			genesisReads++
		}
		if method == "state_queryStorageAt" {
			batches++
			if batches == 2 {
				client.generation.Add(1)
			}
		}
	}
	metadata, runtime := f.native.Meta, f.native.Runtime
	observed, err := readFinalNativeRewardAtV2(t.Context(), f.native, f.head, 521, f.runtime)
	if err != nil || !reflect.DeepEqual(observed, baseline) || genesisReads != 2 || batches != 4 || f.native.Meta != metadata || f.native.Runtime != runtime || f.native.API.RPC != nil {
		t.Fatalf("historical facade hid replacement or joined partial reward data: genesis=%d batches=%d observed=%+v err=%v", genesisReads, batches, observed, err)
	}
}
