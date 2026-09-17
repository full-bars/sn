// Exact producer-shaped carriers get fresh structural admission once, while
// historical RawMessage spellings retain the independent legacy oracle.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

// One deterministic byte census exercises every base64 quantum remainder.
func priorCanonicalPayloadTestV2(t *testing.T, size int) (campaignEvidenceFilePayload, []byte, campaignEvidenceFileEntry) {
	t.Helper()
	raw := make([]byte, size)
	for index := range raw {
		raw[index] = byte(index)
	}
	payload := campaignEvidenceFilePayload{Schema: campaignEvidenceFileSchema, RunID: "prior-canonical-work", Scope: "run", Path: campaignCollectedIndexPathV2, ContentHash: bytesSHA256(raw), Size: uint64(size), Data: raw}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return payload, encoded, campaignEvidenceFileEntry{Path: payload.Path, ContentHash: payload.ContentHash, Size: payload.Size}
}

// Count the actual verifier's work choice, not elapsed time or a supplied
// authentication result. The old unconditional general-Json route fails this.
func TestFinalCaptureCapacityPriorCarrierCanonicalV2UsesFreshCanonicalOwner(t *testing.T) {
	cfg := campaignMetadataConfigTestV2(t)
	limits, err := campaignEvidenceLimitsForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, size := range []int{0, 1, 2, 3, 48*1024 - 1, 48 * 1024, 48*1024 + 1, 96*1024 + 1} {
		payload, encoded, entry := priorCanonicalPayloadTestV2(t, size)
		canonical, err := canonicalFinalPriorFilePayloadV2(limits, payload.RunID, payload.Scope, entry, encoded)
		if err != nil || len(canonical) != len(encoded) || &canonical[0] != &encoded[0] || !json.Valid(canonical) {
			t.Fatalf("canonical owner admission size=%d: %v", size, err)
		}
		envelope, wire := priorCarrierDecodeSignedWireTestV2(t, cfg, key, payload.RunID, encoded)
		entry.EnvelopeHash = envelope.ContentHash
		calls := 0
		err = verifyFinalPriorCarrierWireWithObserverV2(cfg, payload.RunID, payload.Scope, entry, envelope.Signer, wire, func(fast bool) {
			calls++
			if !fast {
				t.Error("canonical carrier repeated general Json verification")
			}
		})
		if err != nil || calls != 1 {
			t.Fatalf("real canonical verifier work size=%d calls=%d: %v", size, calls, err)
		}
		assertPriorCarrierDecodeTestV2(t, "canonical-original", cfg, payload.RunID, entry, envelope.Signer, wire, true)
	}
}

// Equivalent but historically different spellings must fall back; none can
// inherit the producer-specific structural proof or change accepted wire.
func TestFinalCaptureCapacityPriorCarrierCanonicalV2RetainsHistoricalFallback(t *testing.T) {
	cfg := campaignMetadataConfigTestV2(t)
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	payload, encoded, entry := priorCanonicalPayloadTestV2(t, 1)
	dataStart := bytes.Index(encoded, []byte(`,"data":`))
	if dataStart < 0 {
		t.Fatal("fixture has no actual data field")
	}
	for _, body := range [][]byte{
		bytes.Replace(encoded, []byte(`"data"`), []byte(`"DATA"`), 1),
		bytes.Replace(encoded, []byte(`"AA=="`), []byte(`"\u0041A=="`), 1),
		bytes.Replace(encoded, []byte(`"AA=="`), []byte(`"AA\u000a=="`), 1),
		bytes.Replace(encoded, []byte(`"AA=="`), []byte(`[0]`), 1),
		bytes.Replace(encoded, []byte(`"schema":`), []byte(`"schema":"discarded","schema":`), 1),
		[]byte(`{"data":"AA==",` + string(encoded[1:dataStart]) + `}`),
	} {
		envelope, wire := priorCarrierDecodeSignedWireTestV2(t, cfg, key, payload.RunID, body)
		entry.EnvelopeHash = envelope.ContentHash
		calls := 0
		err := verifyFinalPriorCarrierWireWithObserverV2(cfg, payload.RunID, payload.Scope, entry, envelope.Signer, wire, func(fast bool) {
			calls++
			if fast {
				t.Error("historical spelling borrowed canonical owner")
			}
		})
		if err != nil || calls != 1 {
			t.Fatalf("historical real verifier calls=%d: %v", calls, err)
		}
		assertPriorCarrierDecodeTestV2(t, "historical-original", cfg, payload.RunID, entry, envelope.Signer, wire, true)
	}
}

// Base64 decoding ignores CR/LF, but bare controls remain invalid Json. Exact
// source size/hash alone cannot authorize these producer-looking payloads.
func TestFinalCaptureCapacityPriorCarrierCanonicalV2RefusesRawJsonControls(t *testing.T) {
	cfg := campaignMetadataConfigTestV2(t)
	limits, err := campaignEvidenceLimitsForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	payload, encoded, entry := priorCanonicalPayloadTestV2(t, 1)
	for _, control := range []byte{'\r', '\n', '\t', 0, '"', '\\', 0xff} {
		needle := []byte("AA==")
		replacement := []byte{'A', control, 'A', '=', '='}
		body := bytes.Replace(encoded, needle, replacement, 1)
		// Go's Json scanner historically accepts invalid Utf8 inside
		// strings; the base64 decoder must still refuse that byte.
		if bytes.Equal(body, encoded) || json.Valid(body) != (control == 0xff) {
			t.Fatalf("control %02x did not reproduce malformed Json", control)
		}
		canonical, _ := canonicalFinalPriorFilePayloadV2(limits, payload.RunID, payload.Scope, entry, body)
		if canonical != nil {
			t.Fatalf("raw Json control %02x acquired canonical trust", control)
		}
	}
}

// A canonical owner belongs to these bytes in this invocation. Signature,
// routing and complete outer-wire custody are never reused across mutations.
func TestFinalCaptureCapacityPriorCarrierCanonicalV2RetainsFreshAuthentication(t *testing.T) {
	cfg := campaignMetadataConfigTestV2(t)
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	payload, encoded, entry := priorCanonicalPayloadTestV2(t, 3)
	envelope, wire := priorCarrierDecodeSignedWireTestV2(t, cfg, key, payload.RunID, encoded)
	entry.EnvelopeHash = envelope.ContentHash
	assertPriorCarrierDecodeTestV2(t, "initial", cfg, payload.RunID, entry, envelope.Signer, wire, true)
	for _, mutate := range []func(*ReleaseEvidenceEnvelope){
		func(value *ReleaseEvidenceEnvelope) {
			value.Signature = "0x" + strings.Repeat("00", crypto.SignatureLength)
		},
		func(value *ReleaseEvidenceEnvelope) { value.ContentHash = "sha256:" + strings.Repeat("0", 64) },
		func(value *ReleaseEvidenceEnvelope) { value.ChainID++ },
		func(value *ReleaseEvidenceEnvelope) { value.RunID = "foreign" },
		func(value *ReleaseEvidenceEnvelope) {
			value.Payload = bytes.Replace(value.Payload, []byte("AAEC"), []byte("AAED"), 1)
		},
	} {
		changed := *envelope
		changed.Payload = bytes.Clone(envelope.Payload)
		mutate(&changed)
		changedWire, err := json.Marshal(&changed)
		if err != nil {
			t.Fatal(err)
		}
		assertPriorCarrierDecodeTestV2(t, "mutated-authentication", cfg, payload.RunID, entry, envelope.Signer, changedWire, false)
	}
	changed := entry
	changed.EnvelopeHash = "sha256:" + strings.Repeat("0", 64)
	assertPriorCarrierDecodeTestV2(t, "foreign-manifest-envelope", cfg, payload.RunID, changed, envelope.Signer, wire, false)
	for _, changedWire := range [][]byte{append(bytes.Clone(wire), ' '), append([]byte{' '}, wire...), wire[:len(wire)-1]} {
		assertPriorCarrierDecodeTestV2(t, "noncanonical-outer", cfg, payload.RunID, entry, envelope.Signer, changedWire, false)
	}
	assertPriorCarrierDecodeTestV2(t, "fresh-original-again", cfg, payload.RunID, entry, envelope.Signer, wire, true)
}

// Hash-addressed validator controls retain their original typed-source check,
// even when their small outer payload happens to have canonical file framing.
func TestFinalCaptureCapacityPriorCarrierCanonicalV2KeepsTypedSourceBoundary(t *testing.T) {
	cfg := campaignMetadataConfigTestV2(t)
	limits, err := campaignEvidenceLimitsForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	payload, _, entry := priorCanonicalPayloadTestV2(t, 3)
	payload.Path = "final-inputs/validators/v2/" + strings.TrimPrefix(payload.ContentHash, "sha256:") + ".bin"
	entry.Path = payload.Path
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if canonical, err := canonicalFinalPriorFilePayloadV2(limits, payload.RunID, payload.Scope, entry, encoded); err != nil || canonical != nil {
		t.Fatalf("typed validator source inherited ordinary canonical admission: %v", err)
	}
}
