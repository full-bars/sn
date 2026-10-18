// Fork-only automatic import of 3.23-fix saved client identities.
//
// A 3.23-fix provider keeps one saved client JWT per proxy slot in
// .client_jwts.json and signs its client registrations with the raw Ed25519
// seed in .provider.key. The sn miner keeps one credential file per slot
// (.provider.jwt, .provider-<hash>.jwt) and refuses to start on a legacy
// directory until the marker .provider.key.identity proves the retained key.
//
// This path detects the legacy state, proves the retained key still matches
// the key the server holds for each saved client_id (GET /key/<client_id>,
// with a refresh probe when that read is inconclusive), and only then writes
// the per-slot credential files plus the marker. When the proof fails for a
// slot it records the decision in .provider.legacy-import.json and lets the
// normal registration path register a new client for that slot, the same
// fallback 3.23-fix takes when its own reuse fails. It never writes into the
// 3.23-fix files, so starting the old provider again on the same directory
// still reuses the old identities (rollback stays possible).
package miner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"

	"github.com/urfoundation/sn/clientauth"
	"github.com/urfoundation/sn/internal/durablesync"
)

const (
	legacyClientJwtStoreName  = ".client_jwts.json"
	legacyProviderKeyName     = ".provider.key"
	legacyProviderMarkerName  = ".provider.key.identity"
	legacyImportJournalName   = ".provider.legacy-import.json"
	legacyImportJournalSchema = "urnetwork-legacy-import-v1"
	legacyImportDirectKey     = "direct"
)

// A transient proof read (the API was never reached) is retried within the
// start and never journaled as a final decision. Only a verdict freezes a
// fallback.
const legacyImportTransientAttempts = 3

var legacyImportTransientBackoff = 2 * time.Second

// A persistent partial outage (the API answers, but the proof reads stay
// unavailable) is bounded so the provider still starts, minting a new identity
// exactly as the documented fallback: three consecutive answered failures or
// thirty minutes since the first one. Operator decision 2026-10-17 suggests
// exactly these bounds; three starts covers a boot/restart blip and thirty
// minutes covers a single long run. A total outage (the API is unreachable)
// never counts toward the bound and keeps deferring.
const (
	legacyImportDeferralStarts = 3
	legacyImportDeferralWindow = 30 * time.Minute
)

// legacyClientJwtEntry mirrors the 3.23-fix clientJWTEntry
// (provider/client_jwt_store.go:29-34). Only these fields are read; the
// store's extra bytes and any future fields are ignored.
type legacyClientJwtEntry struct {
	ByClientJWT string    `json:"by_client_jwt"`
	ClientID    string    `json:"client_id"`
	NetworkID   string    `json:"network_id"`
	MintedAt    time.Time `json:"minted_at"`
}

// legacyImportJournalSlot freezes one slot's decision so a later start never
// silently switches the slot's identity again.
type legacyImportJournalSlot struct {
	State    string `json:"state"`
	ClientID string `json:"client_id,omitempty"`
	Proof    string `json:"proof,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Target   string `json:"target,omitempty"`
	// A deferred slot is retry bookkeeping, not a decision: the next start
	// proves it again. Only answered partial-outage failures count toward the
	// bound; a total outage records the deferral without counting.
	Deferrals       int   `json:"deferrals,omitempty"`
	FirstDeferredAt int64 `json:"first_deferred_at,omitempty"`
}

type legacyImportJournal struct {
	Schema string                             `json:"schema"`
	Slots  map[string]legacyImportJournalSlot `json:"slots"`
}

func newLegacyImportJournal() legacyImportJournal {
	return legacyImportJournal{Schema: legacyImportJournalSchema, Slots: map[string]legacyImportJournalSlot{}}
}

// legacyImportRecorder receives the operator-visible import lines. The
// production recorder is the provider diagnostics exporter; tests record the
// calls directly.
type legacyImportRecorder interface {
	legacyImportCommitted(clientID8, slot, target string)
	legacyImportFallback(clientID8, slot, reason string)
	legacyImportSkipped(reason, detail string)
	legacyImportSummary(imported, fallback, skipped int)
}

// legacyImportProver answers the two automatic proof questions. The real
// client uses the pinned SDK; tests supply fixed answers.
type legacyImportProver interface {
	// ServerPublicKey returns the public key the server currently holds for
	// clientID, nil when it holds none, or an error when it cannot answer.
	ServerPublicKey(ctx context.Context, clientID connect.Id) ([]byte, error)
	// RefreshAccepted verifies the API still accepts token and preserves its
	// identity on refresh, without persisting anything or allocating.
	RefreshAccepted(ctx context.Context, token string) error
}

// sdkLegacyImportProver is the live prover. ServerPublicKey reads the public
// client key route (server api.go:322, sdk api_sn.go:23-38);
// RefreshAccepted runs the same refresh and identity validation the normal
// retained-credential path runs (clientauth/session.go:232-269).
type sdkLegacyImportProver struct{ api *sdk.Api }

func (self *sdkLegacyImportProver) ServerPublicKey(ctx context.Context, clientID connect.Id) ([]byte, error) {
	sdkID, err := sdk.IdFromBytes(clientID[:])
	if err != nil {
		return nil, err
	}
	result, err := self.api.GetClientKeySyncWithContext(ctx, &sdk.GetClientKeyArgs{ClientId: sdkID})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("server client key read returned no result")
	}
	return result.PublicKey, nil
}

func (self *sdkLegacyImportProver) RefreshAccepted(ctx context.Context, token string) error {
	return clientauth.ProbeClientJwtRefresh(ctx, self.api, token)
}

// newSdkLegacyImportProver builds one direct control-plane API for the whole
// import proof. The proof reads are small public/self reads, like the login
// the operator already runs from this host; a per-slot proxy is deliberately
// not used so a dead proxy slot cannot silently discard the other slots.
func newSdkLegacyImportProver(ctx context.Context, apiUrl string) (legacyImportProver, func(), error) {
	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)
	api := sdk.NewApi(ctx, clientStrategy, apiUrl)
	return &sdkLegacyImportProver{api: api}, func() {
		clientStrategy.Close()
		api.CloseAndWait(context.Background())
	}, nil
}

// legacyImportHooks fail only at real durable write boundaries. Tests use
// them to interrupt a commit and prove the next start recovers.
type legacyImportHooks struct {
	afterMarkerWrite     func() error
	afterCredentialWrite func(storeKey string) error
}

type legacyImportOptions struct {
	stateDir       string
	networkJwtPath string
	// providers mirrors the run's configured slots: empty means the single
	// direct slot, exactly like providerRunSettings.run.
	providers      []*connect.ProxySettings
	disabled       bool
	explicitCreate bool
	explicitAdopt  bool
	recorder       legacyImportRecorder
	newProver      func() (legacyImportProver, func(), error)
	hooks          legacyImportHooks
}

// legacyImportResult tells the run which slots may register a new client.
type legacyImportResult struct {
	mintSlots map[string]bool
}

func (self legacyImportResult) mintAllowed(storeKey string) bool {
	return self.mintSlots[storeKey]
}

// One configured slot considered for import.
type legacyImportSlot struct {
	storeKey  string
	display   string
	target    string
	settings  *connect.ProxySettings
	entry     legacyClientJwtEntry
	clientID  connect.Id
	clientID8 string
	// state is "prove", "commit", "fallback", "defer" or "refuse". A deferred
	// slot made no decision and is retried on the next start.
	state  string
	proof  string
	reason string
	// A deferred slot counts answered partial-outage failures toward the mint
	// bound and remembers when the first one happened. A total-outage failure
	// is marked unreachable and never counts.
	deferrals       int
	firstDeferredAt int64
	unreachable     bool
	// fallbackRecorded marks a sticky fallback loaded from the journal: the
	// decision is replayed quietly, not announced again.
	fallbackRecorded bool
}

// legacyImportStoreKey is the 3.23-fix store key of a configured slot. The
// 3.23-fix store keys by jwtStoreKey (provider/client_jwt_store.go:347-357),
// which is connect.ProxySettings.Key(): the address, or address + \x1f + the
// auth user; the direct worker uses the "direct" sentinel.
func legacyImportStoreKey(settings *connect.ProxySettings) string {
	if settings == nil {
		return legacyImportDirectKey
	}
	return settings.Key()
}

// legacyImportSlotHash is the sn slot hash of a configured proxy: the same
// hash providerClientJwtPath (miner/run.go) resolves through the shared
// providerSlotIdentities rule, so an address carrying two 3.23-fix identities
// maps to two credential files here too.
func legacyImportSlotHash(identities providerSlotIdentities, settings *connect.ProxySettings) [32]byte {
	return identities.hash(settings)
}

// legacyImportTargetPath is the sn per-slot credential path for a configured
// slot: the same name providerClientJwtPath (miner/run.go) resolves in the
// state directory, kept next to this logic so it is testable without the
// process-wide state dir resolution.
func legacyImportTargetPath(stateDir string, identities providerSlotIdentities, settings *connect.ProxySettings) string {
	if settings == nil {
		return filepath.Join(stateDir, ".provider.jwt")
	}
	sum := legacyImportSlotHash(identities, settings)
	return filepath.Join(stateDir, fmt.Sprintf(".provider-%x.jwt", sum[:8]))
}

// legacyImportDisplaySlot names a slot without credentials. A proxy is shown
// by address only; the store's \x1f user separator never reaches a log line.
func legacyImportDisplaySlot(settings *connect.ProxySettings) string {
	if settings == nil {
		return "direct"
	}
	return "proxy:" + settings.Address
}

func legacyImportClientID8(id connect.Id) string {
	value := id.String()
	if len(value) > 8 {
		return value[:8]
	}
	return value
}

// nopLegacyImportRecorder keeps a nil recorder silent instead of panicking.
type nopLegacyImportRecorder struct{}

func (nopLegacyImportRecorder) legacyImportCommitted(string, string, string) {}
func (nopLegacyImportRecorder) legacyImportFallback(string, string, string)  {}
func (nopLegacyImportRecorder) legacyImportSkipped(string, string)           {}
func (nopLegacyImportRecorder) legacyImportSummary(int, int, int)            {}

// runLegacyProviderImport performs one startup's detection, proof and commit.
// It returns the slots that may register a new client because the saved
// identity could not be reused.
func runLegacyProviderImport(ctx context.Context, opts legacyImportOptions) legacyImportResult {
	result := legacyImportResult{mintSlots: map[string]bool{}}
	if opts.recorder == nil {
		opts.recorder = nopLegacyImportRecorder{}
	}
	if opts.disabled || opts.explicitCreate || opts.explicitAdopt || opts.stateDir == "" {
		return result
	}
	skipped := 0
	skip := func(reason, detail string) {
		skipped++
		opts.recorder.legacyImportSkipped(reason, detail)
	}

	// Detection: a 3.23-fix store AND the retained seed. Without both, this is
	// not the migration case and nothing here may act.
	storePath := filepath.Join(opts.stateDir, legacyClientJwtStoreName)
	storeRaw, err := os.ReadFile(storePath)
	if errors.Is(err, os.ErrNotExist) {
		return result
	}
	if err != nil {
		skip("store_unreadable", err.Error())
		return result
	}
	var store map[string]legacyClientJwtEntry
	if json.Unmarshal(storeRaw, &store) != nil || len(store) == 0 {
		// One line, no writes, no panic; the normal startup gate decides what
		// happens to the directory.
		skip("store_malformed", storePath)
		return result
	}

	seed, err := os.ReadFile(filepath.Join(opts.stateDir, legacyProviderKeyName))
	if errors.Is(err, os.ErrNotExist) {
		return result
	}
	if err != nil {
		skip("provider_key_unreadable", err.Error())
		return result
	}
	marker, markerErr := clientauth.ProviderKeyIdentityMarker(seed)
	if markerErr != nil {
		skip("provider_key_invalid", markerErr.Error())
		return result
	}

	// The marker is the key evidence sn already adopted this seed. When it is
	// absent, any sn credential or registration custody in the directory means
	// an earlier sn operation owned this key; adopting it automatically on top
	// of that history is not this import's call.
	markerPath := filepath.Join(opts.stateDir, legacyProviderMarkerName)
	markerRaw, markerReadErr := os.ReadFile(markerPath)
	markerExists := markerReadErr == nil
	if markerExists && !bytes.Equal(markerRaw, marker) {
		skip("provider_marker_mismatch", markerPath)
		return result
	}
	if !markerExists {
		if custody := legacySnCustodyName(opts.stateDir); custody != "" {
			skip("sn_custody_without_marker", custody)
			return result
		}
	}

	// The saved identities are only reusable inside the signed-in network.
	networkToken, err := clientauth.ReadToken(opts.networkJwtPath)
	if err != nil {
		skip("network_sign_in_unreadable", err.Error())
		return result
	}
	networkID, err := clientauth.JwtNetworkId(networkToken)
	if err != nil || networkID == "" {
		skip("network_sign_in_identity_invalid", opts.networkJwtPath)
		return result
	}

	journal, err := readLegacyImportJournal(opts.stateDir)
	if err != nil {
		skip("journal_invalid", err.Error())
		return result
	}

	// Map the configured slots. Entries whose slot is not configured in this
	// run stay untouched, and a slot that already has an sn credential is
	// never a candidate.
	providers := opts.providers
	if len(providers) == 0 {
		providers = []*connect.ProxySettings{nil}
	}
	identities := newProviderSlotIdentities(providers)
	var slots []*legacyImportSlot
	for _, settings := range providers {
		storeKey := legacyImportStoreKey(settings)
		target := legacyImportTargetPath(opts.stateDir, identities, settings)
		if _, err := os.Stat(target); err == nil {
			continue
		}
		entry, present := store[storeKey]
		if !present {
			continue
		}
		slot := &legacyImportSlot{
			storeKey: storeKey,
			display:  legacyImportDisplaySlot(settings),
			target:   target,
			settings: settings,
			entry:    entry,
			state:    "prove",
		}
		// The saved client_id is the display identity even when its entry
		// later fails validation, so the fallback warning can name it.
		if len(entry.ClientID) >= 8 {
			slot.clientID8 = entry.ClientID[:8]
		}
		if recorded, ok := journal.Slots[storeKey]; ok {
			switch recorded.State {
			case "imported":
				// The slot's choice was the saved identity and its file is
				// gone. Re-importing could resurrect a deliberately removed
				// identity, so the slot is left to the normal startup gate.
				continue
			case "fallback":
				// Sticky: never import this slot again. The new client is
				// registered by the normal durable path below.
				slot.state = "fallback"
				slot.fallbackRecorded = true
				slot.reason = recorded.Reason
				if slot.reason == "" {
					slot.reason = "import_declined_earlier"
				}
			case "defer":
				// Retry bookkeeping, not a decision: prove again, keeping the
				// partial-outage count so the bound survives restarts.
				slot.deferrals = recorded.Deferrals
				slot.firstDeferredAt = recorded.FirstDeferredAt
			}
		}
		slots = append(slots, slot)
	}

	// Validate every candidate entry before any proof or write.
	for _, slot := range slots {
		if slot.state != "prove" {
			continue
		}
		clientID, reason, ok := legacyEntryIdentity(slot.entry, networkID)
		if !ok {
			slot.state = "fallback"
			slot.reason = reason
			continue
		}
		slot.clientID = clientID
		slot.clientID8 = legacyImportClientID8(clientID)
	}

	// Proof phase. Tier 1 compares the retained public key with the key the
	// server holds for the client_id. A null or unreachable read is
	// inconclusive, not a refusal, and falls through to tier 2: the same
	// refresh the normal retained-credential path runs. Only a completed
	// refresh that preserves the identity counts.
	var prove []*legacyImportSlot
	for _, slot := range slots {
		if slot.state == "prove" {
			prove = append(prove, slot)
		}
	}
	if len(prove) > 0 {
		prover, closeProver, err := opts.newProver()
		if err != nil {
			skip("proof_unavailable", err.Error())
			return result
		}
		public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
		var inconclusive []*legacyImportSlot
		for _, slot := range prove {
			serverKey, err := prover.ServerPublicKey(ctx, slot.clientID)
			switch {
			case err != nil:
				slot.reason = "server_key_unavailable"
				inconclusive = append(inconclusive, slot)
			case len(serverKey) == 0:
				slot.reason = "server_key_absent"
				inconclusive = append(inconclusive, slot)
			case !bytes.Equal(serverKey, public):
				slot.state = "fallback"
				slot.reason = "server_key_differs"
			default:
				slot.state = "commit"
				slot.proof = "server_key"
			}
		}
		for _, slot := range inconclusive {
			// A transient read (the API was never reached) is retried within
			// this start. If it stays unavailable the slot is deferred: the
			// saved credential stays untouched, nothing is journaled, no new
			// client is allocated, and the next start proves it again. Only a
			// completed refusal is a fallback.
			slot.state = "defer"
			slot.reason = "refresh_unavailable"
			var lastErr error
			for attempt := 1; ; attempt++ {
				err := prover.RefreshAccepted(ctx, slot.entry.ByClientJWT)
				if err == nil {
					slot.state = "commit"
					slot.proof = "refresh"
					slot.reason = ""
					break
				}
				lastErr = err
				if !legacyImportProofRetryable(err) {
					slot.state = "fallback"
					slot.reason = "refresh_rejected"
					break
				}
				if ctx.Err() != nil || attempt >= legacyImportTransientAttempts {
					break
				}
				select {
				case <-ctx.Done():
					return result
				case <-time.After(legacyImportTransientBackoff):
				}
			}
			if slot.state == "defer" {
				slot.unreachable = legacyImportProofUnreachable(lastErr)
			}
		}
		closeProver()
	}
	if ctx.Err() != nil {
		// A canceled startup makes no permanent identity decision.
		return result
	}

	// Bound a persistent partial outage: when the API answers but the proof
	// stays unavailable, three answered failed starts or thirty minutes since
	// the first one fall back to the documented mint. A total outage never
	// counts and keeps deferring.
	now := time.Now()
	for _, slot := range slots {
		if slot.state != "defer" || slot.unreachable {
			continue
		}
		if slot.firstDeferredAt == 0 {
			slot.firstDeferredAt = now.Unix()
		}
		slot.deferrals++
		if slot.deferrals >= legacyImportDeferralStarts ||
			now.Unix()-slot.firstDeferredAt >= int64(legacyImportDeferralWindow/time.Second) {
			slot.state = "fallback"
			slot.reason = "proof_unavailable_bound"
		}
	}

	// Deferred slots made no decision: the saved credential stays untouched
	// and the next start proves it again.
	for _, slot := range slots {
		if slot.state == "defer" {
			skip("refresh_unavailable", slot.clientID8)
		}
	}

	// A fallback may never mint over sn custody: an existing registration
	// record, anchor or rejection marker is sn's own durable identity for the
	// slot and the normal path owns it (guardrail i).
	for _, slot := range slots {
		if slot.state != "fallback" {
			continue
		}
		if custody := legacySlotCustodyName(opts.stateDir, slot.target); custody != "" {
			slot.state = "refuse"
			skip("sn_custody_present", custody)
		}
	}

	var commit, fallback, deferred []*legacyImportSlot
	for _, slot := range slots {
		switch slot.state {
		case "commit":
			commit = append(commit, slot)
		case "fallback":
			fallback = append(fallback, slot)
		case "defer":
			deferred = append(deferred, slot)
		}
	}
	if len(commit) == 0 && len(fallback) == 0 && len(deferred) == 0 {
		return result
	}

	// Commit step 1: the marker. Writing it is the adoption semantics the
	// manual --adopt-legacy-provider-key asserts; here the proof above is the
	// assertion. A new client registered for a fallback slot uses this same
	// retained seed, and the old providers ignore the marker entirely. A
	// deferral alone writes nothing.
	if (len(commit) > 0 || len(fallback) > 0) && !markerExists {
		if err := writeLegacyImportNewFile(markerPath, marker); err != nil && !errors.Is(err, os.ErrExist) {
			skip("marker_write_failed", err.Error())
			return result
		}
		if opts.hooks.afterMarkerWrite != nil {
			if err := opts.hooks.afterMarkerWrite(); err != nil {
				return result
			}
		}
	}

	// Commit step 2: the proven credentials. Never an overwrite, one file per
	// slot, atomically.
	imported := 0
	for _, slot := range commit {
		data := []byte(strings.TrimSpace(slot.entry.ByClientJWT))
		if err := writeLegacyImportNewFile(slot.target, data); err != nil {
			if errors.Is(err, os.ErrExist) {
				skip("credential_exists", filepath.Base(slot.target))
			} else {
				skip("credential_write_failed", filepath.Base(slot.target)+": "+err.Error())
			}
			continue
		}
		imported++
		journal.Slots[slot.storeKey] = legacyImportJournalSlot{
			State:    "imported",
			ClientID: slot.entry.ClientID,
			Proof:    slot.proof,
			Target:   filepath.Base(slot.target),
		}
		opts.recorder.legacyImportCommitted(slot.clientID8, slot.display, filepath.Base(slot.target))
		if opts.hooks.afterCredentialWrite != nil {
			if err := opts.hooks.afterCredentialWrite(slot.storeKey); err != nil {
				// The state reached here is the state of a killed import:
				// the marker and the earlier credentials are durable, the
				// rest is redone by the next start.
				return result
			}
		}
	}

	// Commit step 3: the fallback decisions. The journal freezes them so a
	// later start never imports these slots again. A sticky fallback loaded
	// from the journal keeps the mint allowed but is not announced again.
	newFallbacks := 0
	for _, slot := range fallback {
		journal.Slots[slot.storeKey] = legacyImportJournalSlot{
			State:    "fallback",
			ClientID: slot.entry.ClientID,
			Reason:   slot.reason,
			Target:   filepath.Base(slot.target),
		}
		result.mintSlots[slot.storeKey] = true
		if !slot.fallbackRecorded {
			newFallbacks++
			opts.recorder.legacyImportFallback(slot.clientID8, slot.display, slot.reason)
		}
	}

	// Commit step 4: deferral bookkeeping. This is not a decision: the saved
	// credential stays untouched and the count only bounds a partial outage.
	for _, slot := range deferred {
		journal.Slots[slot.storeKey] = legacyImportJournalSlot{
			State:           "defer",
			ClientID:        slot.entry.ClientID,
			Reason:          slot.reason,
			Target:          filepath.Base(slot.target),
			Deferrals:       slot.deferrals,
			FirstDeferredAt: slot.firstDeferredAt,
		}
	}
	if err := writeLegacyImportJournal(opts.stateDir, journal); err != nil {
		// The mint may proceed without the audit record, but an unreadable
		// journal must not silently become a decision again.
		skip("journal_write_failed", err.Error())
	}
	if imported > 0 || newFallbacks > 0 || skipped > 0 {
		opts.recorder.legacyImportSummary(imported, newFallbacks, skipped)
	}
	return result
}

// legacyImportProofRetryable reports whether a proof error is a transient
// unavailable read rather than a verdict about the saved identity. The API was
// never reached, so no permanent decision may be based on it.
func legacyImportProofRetryable(err error) bool {
	return clientauth.IsRegistrationRefreshUnavailable(err) || legacyImportProofUnreachable(err)
}

// legacyImportProofUnreachable reports whether the proof error chain shows the
// API was never reached: DNS, dial, connection or timeout errors. A total
// outage defers indefinitely rather than minting, because a saved identity the
// server may still accept must not be discarded while nothing can be proven.
func legacyImportProofUnreachable(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ETIMEDOUT)
}

// legacyEntryIdentity applies the safety checks that precede any import:
// the token parses, the client_id claim matches the stored client_id, and
// both the entry and the token name the signed-in network. Expiry is not
// checked here: an expired retained credential is handled exactly like any
// other retained credential (a refresh at startup decides), never renewed
// by this path.
func legacyEntryIdentity(entry legacyClientJwtEntry, networkID string) (connect.Id, string, bool) {
	token := strings.TrimSpace(entry.ByClientJWT)
	if token == "" {
		return connect.Id{}, "client_jwt_missing", false
	}
	entryClientID, err := connect.ParseId(entry.ClientID)
	if err != nil || entryClientID.String() != entry.ClientID {
		return connect.Id{}, "client_id_invalid", false
	}
	claimClientID, err := clientauth.ClientIdFromJwt(token)
	if err != nil {
		return connect.Id{}, "client_jwt_invalid", false
	}
	if claimClientID != entryClientID {
		return connect.Id{}, "client_id_mismatch", false
	}
	tokenNetworkID, err := clientauth.JwtNetworkId(token)
	if err != nil || entry.NetworkID != networkID || tokenNetworkID != networkID {
		return connect.Id{}, "network_id_mismatch", false
	}
	return entryClientID, "", true
}

// legacySnCustodyName reports the first sn credential or registration custody
// file in the state directory, or "" when none exists. The 3.23-fix files
// (.provider.key, .provider.cert, .provider_version, .client_jwts.json) are
// not custody.
func legacySnCustodyName(stateDir string) string {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if legacySnCustodyFileName(entry.Name()) {
			return entry.Name()
		}
	}
	return ""
}

// legacySlotCustodyName is legacySnCustodyName restricted to one slot's
// target credential name.
func legacySlotCustodyName(stateDir, target string) string {
	base := filepath.Base(target)
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == base || strings.HasPrefix(name, base+".") {
			return name
		}
	}
	return ""
}

// legacySnCustodyFileName recognizes the files an sn credential or
// registration leaves for a slot: the credential itself, the durable
// registration record and its anchor, the existing-client proof, the owner
// lock and the rejection marker.
func legacySnCustodyFileName(name string) bool {
	if name == ".provider.jwt" {
		return true
	}
	if strings.HasPrefix(name, ".provider-") && strings.HasSuffix(name, ".jwt") {
		value := strings.TrimSuffix(strings.TrimPrefix(name, ".provider-"), ".jwt")
		if raw, err := hex.DecodeString(value); err == nil && len(raw) == 8 && value == hex.EncodeToString(raw) {
			return true
		}
	}
	for _, suffix := range []string{".jwt.registration", ".jwt.registration.started", ".jwt.registration.existing", ".jwt.registration.lock", ".jwt.rejected"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// writeLegacyImportNewFile publishes data at path atomically, mode 0600, and
// never replaces an existing file: a named temporary is fsynced, linked into
// place (an existing target is fs.ErrExist) and the directory is fsynced.
func writeLegacyImportNewFile(path string, data []byte) (returnErr error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if returnErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpPath, path); err != nil {
		return err
	}
	// The link is the publish; the temporary name is now redundant.
	_ = os.Remove(tmpPath)
	return durablesync.SyncParent(path)
}

func writeLegacyImportJournal(stateDir string, journal legacyImportJournal) error {
	raw, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	path := filepath.Join(stateDir, legacyImportJournalName)
	tmp, err := os.CreateTemp(stateDir, "."+legacyImportJournalName+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return durablesync.SyncParent(path)
}

// readLegacyImportJournal returns the journal, empty when it does not exist.
// A corrupt or unknown journal is an error: its decisions are identity
// decisions and guessing them is worse than not acting.
func readLegacyImportJournal(stateDir string) (legacyImportJournal, error) {
	empty := newLegacyImportJournal()
	raw, err := os.ReadFile(filepath.Join(stateDir, legacyImportJournalName))
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	var journal legacyImportJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		return empty, err
	}
	if journal.Schema != legacyImportJournalSchema || journal.Slots == nil {
		return empty, errors.New("legacy import journal has an unknown schema")
	}
	for _, slot := range journal.Slots {
		if slot.State != "imported" && slot.State != "fallback" && slot.State != "defer" {
			return empty, errors.New("legacy import journal has an unknown slot state")
		}
	}
	return journal, nil
}
