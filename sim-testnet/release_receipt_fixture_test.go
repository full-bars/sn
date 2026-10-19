// Receipt fixtures commit to real empty or single-extrinsic bodies and hash
// complete SDK SCALE headers, as crv4's canonical receipt admission requires.
package main

import (
	"fmt"
	"testing"

	"github.com/centrifuge/go-substrate-rpc-client/v4/types"
	"github.com/centrifuge/go-substrate-rpc-client/v4/types/codec"
	"golang.org/x/crypto/blake2b"
)

// The one-entry layout0 root is an independent exact leaf codec, not a call to
// the production trie implementation. Fixtures need only empty or one entry.
func releaseReceiptTestHeader(t *testing.T, parent types.Hash, number uint64, extrinsics ...string) (types.Header, types.Hash) {
	t.Helper()
	if len(extrinsics) > 1 {
		t.Fatal("receipt fixture supports only empty or single-extrinsic bodies")
	}
	trie := []byte{0}
	if len(extrinsics) == 1 {
		raw, err := codec.HexDecodeString(extrinsics[0])
		if err != nil || len(raw) == 0 {
			t.Fatalf("invalid receipt fixture extrinsic: %v", err)
		}
		length, err := codec.Encode(types.NewUCompactFromUInt(uint64(len(raw))))
		if err != nil {
			t.Fatal(err)
		}
		// Leaf + two compact-index-zero nibbles, then SCALE Vec<u8> value.
		trie = append(append([]byte{0x42, 0}, length...), raw...)
	}
	header := types.Header{ParentHash: parent, Number: types.BlockNumber(number), StateRoot: types.Hash{3}, ExtrinsicsRoot: types.Hash(blake2b.Sum256(trie)), Digest: types.Digest{}}
	raw, err := codec.Encode(header)
	if err != nil {
		t.Fatal(err)
	}
	return header, types.Hash(blake2b.Sum256(raw))
}

// Spells the header as Substrate's JSON does; the pinned SDK's marshaler omits
// the quantity's required 0x prefix.
func releaseReceiptTestHeaderWire(header types.Header) map[string]any {
	return map[string]any{
		"parentHash": header.ParentHash.Hex(), "number": fmt.Sprintf("0x%x", uint64(header.Number)),
		"stateRoot": header.StateRoot.Hex(), "extrinsicsRoot": header.ExtrinsicsRoot.Hex(),
		"digest": map[string]any{"logs": []string{}},
	}
}

// The chain_getBlock reply for the exact body the header commits to.
func releaseReceiptTestBlockWire(header types.Header, extrinsics ...string) map[string]any {
	return map[string]any{"block": map[string]any{"header": releaseReceiptTestHeaderWire(header), "extrinsics": append([]string{}, extrinsics...)}}
}
