package miner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"

	"github.com/urfoundation/sn/clientauth"
)

// A capture launch declares the client every slot must present, so the slot
// that cannot present it is refused at the decision rather than at the device
// construction that compares the identity.
func TestCaptureLaunchIdentityRefusalNamesTheRequiredClient(t *testing.T) {
	recorded := connect.Id{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xf0, 0x01}
	work := &ProviderWorkCaptureProfile{Providers: []ProviderWorkCaptureOwner{{Slot: "direct", ClientId: [16]byte(recorded)}}}
	contractSlot := "proxy-sha256:" + strings.Repeat("ab", 32)
	contract := &ProviderContractCaptureProfile{Providers: []ProviderContractCaptureOwner{{Slot: contractSlot, ClientId: [16]byte{0x21}}}}

	for _, test := range []struct {
		name     string
		work     *ProviderWorkCaptureProfile
		contract *ProviderContractCaptureProfile
		slot     string
		names    string
	}{
		{name: "work profile", work: work, slot: "direct", names: recorded.String()},
		{name: "contract profile", contract: contract, slot: contractSlot, names: connect.Id{0x21}.String()},
		{name: "slot outside the roster", work: work, slot: contractSlot},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := captureLaunchIdentityRefusal(test.work, test.contract, test.slot)
			if err == nil {
				t.Fatal("capture launch permitted a registration")
			}
			if !strings.Contains(err.Error(), test.slot) || !strings.Contains(err.Error(), "registering a replacement is refused") {
				t.Fatalf("refusal = %q, want the slot and the replacement it refuses", err)
			}
			if test.names != "" && !strings.Contains(err.Error(), test.names) {
				t.Fatalf("refusal = %q, want the recorded client %s", err, test.names)
			}
			if test.names == "" && strings.Contains(err.Error(), recorded.String()) {
				t.Fatalf("refusal = %q, want no client identity for an undeclared slot", err)
			}
		})
	}

	if err := captureLaunchIdentityRefusal(nil, nil, "direct"); err != nil {
		t.Fatalf("an ordinary launch was refused: %v", err)
	}
	// The requirement handed to registration carries exactly the recorded
	// client, and an ordinary launch hands down none.
	if got := captureLaunchRetainedIdentity(work, nil, "direct"); got == nil || got.ClientId != recorded {
		t.Fatalf("capture launch retained identity = %+v, want the recorded client %v", got, recorded)
	}
	if got := captureLaunchRetainedIdentity(work, nil, contractSlot); got == nil || got.ClientId != (connect.Id{}) {
		t.Fatalf("capture launch slot outside the roster retained identity = %+v, want no client", got)
	}
	if got := captureLaunchRetainedIdentity(nil, nil, "direct"); got != nil {
		t.Fatalf("an ordinary launch carried a retained identity: %+v", got)
	}
}

// The operator's standing permission and the import's fallback both lose to the
// capture launch: its identity is retained, not replaceable.
func TestProviderSlotCreatePermittedWithholdsCreationUnderCapture(t *testing.T) {
	refusal := errors.New("capture launch slot \"direct\" must present its retained client identity")
	for _, test := range []struct {
		name       string
		flag       bool
		importMint bool
		refusal    error
		want       bool
	}{
		{name: "ordinary launch", flag: true, want: true},
		{name: "ordinary launch from the import", importMint: true, want: true},
		{name: "capture launch", flag: true, refusal: refusal},
		{name: "capture launch whose import decided to mint", flag: true, importMint: true, refusal: refusal},
		{name: "creation off", refusal: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := providerSlotCreatePermitted(test.flag, test.importMint, test.refusal); got != test.want {
				t.Fatalf("creation permitted = %v, want %v", got, test.want)
			}
		})
	}
}

// providerCaptureLaunchRegister runs one slot's real client-identity attempt
// against the synthetic registration server, with the creation authority the
// launch computed and the client identity it must present, if any.
func providerCaptureLaunchRegister(t *testing.T, fixture *providerRegistrationFixture, allowCreate bool, retained *clientauth.RetainedClientIdentity) (connect.Id, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	strategy := connect.NewClientStrategy(ctx, connect.DefaultClientStrategySettings())
	defer strategy.Close()
	api := sdk.NewApi(ctx, strategy, fixture.server.URL)
	defer func() {
		if err := api.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	keyOwner, err := clientauth.OpenProviderClientKey(ctx, filepath.Join(fixture.dir, ".provider.key"), clientauth.ProviderClientKeyOptions{AllowCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := keyOwner.Close(); err != nil {
			t.Error(err)
		}
	}()
	_, clientId, err := keyOwner.LoadOrRegisterClientJwtObserved(ctx, api, filepath.Join(fixture.dir, "jwt"), filepath.Join(fixture.dir, ".provider.jwt"), "direct", allowCreate, retained, nil)
	return clientId, err
}

// A capture slot that kept its key but lost its saved client reaches the
// create path, and that path is where the replacement used to be registered and
// written over the slot's credential before the device compared the identity.
func TestCaptureSlotWithoutRetainedCredentialRegistersNothing(t *testing.T) {
	recorded := connect.Id{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xf0, 0x01}
	profile := &ProviderWorkCaptureProfile{Providers: []ProviderWorkCaptureOwner{{Slot: "direct", ClientId: [16]byte(recorded)}}}
	refusal := captureLaunchIdentityRefusal(profile, nil, "direct")

	capture := newProviderRegistrationFixture(t)
	before, _, _, _ := capture.counts()
	_, err := providerCaptureLaunchRegister(t, capture, providerSlotCreatePermitted(true, false, refusal), nil)
	var refused *clientauth.RegistrationRefusedError
	if err == nil || !errors.As(err, &refused) || refused.Code != "client_registration_disabled" {
		t.Fatalf("capture slot without a retained credential: err = %v, want a registration refusal", err)
	}
	after, _, allocations, _ := capture.counts()
	if after != before || allocations != 0 {
		t.Fatalf("capture slot reached the registration endpoint (%d -> %d posts, %d allocations)", before, after, allocations)
	}
	if _, err := os.Stat(filepath.Join(capture.dir, ".provider.jwt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture slot left a replacement credential behind: %v", err)
	}

	// The same custody with the capture gate removed registers a client that is
	// not the one the profile recorded, which is the replacement the launch has
	// to refuse.
	control := newProviderRegistrationFixture(t)
	before, _, _, _ = control.counts()
	allocated, err := providerCaptureLaunchRegister(t, control, providerSlotCreatePermitted(true, false, nil), nil)
	if err != nil {
		t.Fatalf("control launch: %v", err)
	}
	if allocated == (connect.Id{}) || allocated == recorded {
		t.Fatalf("control launch allocated %v, want a client other than the recorded %v", allocated, recorded)
	}
	after, _, allocations, _ = control.counts()
	if after == before || allocations == 0 {
		t.Fatalf("control launch did not reach the registration endpoint (%d -> %d posts, %d allocations)", before, after, allocations)
	}
	if _, err := os.Stat(filepath.Join(control.dir, ".provider.jwt")); err != nil {
		t.Fatalf("control launch did not persist its credential: %v", err)
	}
}

// A capture slot whose earlier attempt wrote its durable operation and stopped
// before the server bound a client must not submit that operation: the server
// would allocate a client the profile never recorded and the write would
// replace the slot's custody. The refusal names the client it had to present.
func TestCaptureSlotWithUnboundRegistrationRecordSubmitsNothing(t *testing.T) {
	recorded := connect.Id{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xf0, 0x01}
	profile := &ProviderWorkCaptureProfile{Providers: []ProviderWorkCaptureOwner{{Slot: "direct", ClientId: [16]byte(recorded)}}}

	// The first launch reaches the real registration request and loses its
	// reply. The operation is retained without a bound client, which is the
	// window a capture launch must resume as a presentation, never a creation.
	interrupted := newProviderRegistrationFixture(t)
	interrupted.malformed = true
	if err := interrupted.run(t.Context(), true, providerRegistrationHooks{}); err == nil {
		t.Fatal("interrupted launch bound a client from a malformed reply")
	}
	recordPath := filepath.Join(interrupted.dir, ".provider.jwt.registration")
	raw, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("interrupted launch left no durable registration record: %v", err)
	}
	var record providerRegistrationTestRecord
	if err := json.Unmarshal(raw, &record); err != nil || record.ClientId != "" || record.DeviceId != "" {
		t.Fatalf("interrupted launch record = %s, want an unbound operation (%v)", raw, err)
	}
	if _, err := os.Stat(filepath.Join(interrupted.dir, ".provider.jwt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted launch installed a credential: %v", err)
	}

	// A valid reply is available now, so a submitted operation would complete
	// and leave this slot holding a client the profile never recorded.
	interrupted.stateLock.Lock()
	interrupted.malformed = false
	interrupted.stateLock.Unlock()
	posts, legacy, allocations, _ := interrupted.counts()
	_, err = providerCaptureLaunchRegister(t, interrupted, providerSlotCreatePermitted(true, false, captureLaunchIdentityRefusal(profile, nil, "direct")), captureLaunchRetainedIdentity(profile, nil, "direct"))
	var refused *clientauth.RegistrationRefusedError
	if err == nil || !errors.As(err, &refused) || refused.Code != "retained_client_unbound" {
		t.Fatalf("capture slot with an unbound record: err = %v, want the retained-identity refusal", err)
	}
	if !strings.Contains(err.Error(), recorded.String()) {
		t.Fatalf("refusal = %q, want the required client %s named", err, recorded)
	}
	afterPosts, afterLegacy, afterAllocations, _ := interrupted.counts()
	if afterPosts != posts || afterLegacy != legacy || afterAllocations != allocations {
		t.Fatalf("capture slot reached the registration endpoint (%d -> %d posts, %d -> %d allocations)", posts, afterPosts, allocations, afterAllocations)
	}
	if _, err := os.Stat(filepath.Join(interrupted.dir, ".provider.jwt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture slot wrote a credential: %v", err)
	}
	raw, err = os.ReadFile(recordPath)
	if err != nil || json.Unmarshal(raw, &record) != nil || record.ClientId != "" || record.DeviceId != "" {
		t.Fatalf("capture slot changed the retained operation: %s (%v)", raw, err)
	}
}

// providerCaptureBoundCustody runs a launch that is not a capture and stops it
// after its authenticated handoff, then removes the credential. What remains is
// a durable operation the server bound to a client, with nothing on disk that
// can present that client: the custody a capture launch has to resume rather
// than replace.
func providerCaptureBoundCustody(t *testing.T, fixture *providerRegistrationFixture) connect.Id {
	t.Helper()
	stop := errors.New("synthetic bound-custody handoff")
	var bound connect.Id
	if err := fixture.run(t.Context(), true, providerRegistrationHooks{afterAuthenticated: func(_ string, id connect.Id, _ []byte) error {
		bound = id
		return stop
	}}); !errors.Is(err, stop) {
		t.Fatalf("fixture did not complete a bound registration: %v", err)
	}
	if bound == (connect.Id{}) {
		t.Fatal("fixture completed without binding a client")
	}
	recordPath := filepath.Join(fixture.dir, ".provider.jwt.registration")
	var record providerRegistrationTestRecord
	raw, err := os.ReadFile(recordPath)
	if err != nil || json.Unmarshal(raw, &record) != nil || record.ClientId != bound.String() || record.DeviceId == "" {
		t.Fatalf("fixture record = %s, want a binding to %v (%v)", raw, bound, err)
	}
	if err := os.Remove(filepath.Join(fixture.dir, ".provider.jwt")); err != nil {
		t.Fatal(err)
	}
	return bound
}

// A durable operation bound to one client is custody for that client. A capture
// launch whose profile recorded a different one must not submit it: the server
// would complete the operation and the launch would write the other client's
// credential, which the device layer only notices after the write. The refusal
// happens before the request and names the client the profile recorded.
func TestCaptureSlotWithRegistrationRecordBoundToAnotherClientSubmitsNothing(t *testing.T) {
	recorded := connect.Id{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xf0, 0x01}
	profile := &ProviderWorkCaptureProfile{Providers: []ProviderWorkCaptureOwner{{Slot: "direct", ClientId: [16]byte(recorded)}}}

	fixture := newProviderRegistrationFixture(t)
	bound := providerCaptureBoundCustody(t, fixture)
	if bound == recorded {
		t.Fatalf("fixture bound the client the profile records (%v)", bound)
	}
	recordPath := filepath.Join(fixture.dir, ".provider.jwt.registration")
	before, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	posts, legacy, allocations, _ := fixture.counts()
	_, err = providerCaptureLaunchRegister(t, fixture, providerSlotCreatePermitted(true, false, captureLaunchIdentityRefusal(profile, nil, "direct")), captureLaunchRetainedIdentity(profile, nil, "direct"))
	var refused *clientauth.RegistrationRefusedError
	if err == nil || !errors.As(err, &refused) || refused.Code != "retained_client_mismatch" {
		t.Fatalf("capture slot bound to %v: err = %v, want the retained-client mismatch refusal", bound, err)
	}
	if refused.ExpectedClientId != recorded.String() || !strings.Contains(err.Error(), recorded.String()) {
		t.Fatalf("refusal %q names %q, want the recorded client %s", err, refused.ExpectedClientId, recorded)
	}
	if strings.Contains(err.Error(), bound.String()) {
		t.Fatalf("refusal = %q, want the client this slot could not present left out of the requirement", err)
	}
	afterPosts, afterLegacy, afterAllocations, _ := fixture.counts()
	if afterPosts != posts || afterLegacy != legacy || afterAllocations != allocations {
		t.Fatalf("capture slot reached the registration endpoint (%d -> %d posts, %d -> %d allocations)", posts, afterPosts, allocations, afterAllocations)
	}
	if _, err := os.Stat(filepath.Join(fixture.dir, ".provider.jwt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture slot wrote a credential: %v", err)
	}
	after, err := os.ReadFile(recordPath)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("capture slot changed the retained operation: %s (%v)", after, err)
	}
}

// The same bound custody is a legitimate capture resume when the profile
// recorded the client the operation is bound to: the launch replays that
// operation and installs the credential the server already bound to it.
func TestCaptureSlotWithRegistrationRecordBoundToTheRecordedClientResumes(t *testing.T) {
	fixture := newProviderRegistrationFixture(t)
	bound := providerCaptureBoundCustody(t, fixture)
	profile := &ProviderWorkCaptureProfile{Providers: []ProviderWorkCaptureOwner{{Slot: "direct", ClientId: [16]byte(bound)}}}
	recordPath := filepath.Join(fixture.dir, ".provider.jwt.registration")
	before, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	posts, legacy, allocations, _ := fixture.counts()
	clientId, err := providerCaptureLaunchRegister(t, fixture, providerSlotCreatePermitted(true, false, captureLaunchIdentityRefusal(profile, nil, "direct")), captureLaunchRetainedIdentity(profile, nil, "direct"))
	if err != nil {
		t.Fatalf("capture slot matching its bound record: %v", err)
	}
	if clientId != bound {
		t.Fatalf("capture resume presented %v, want the bound %v", clientId, bound)
	}
	afterPosts, afterLegacy, afterAllocations, _ := fixture.counts()
	if afterPosts <= posts || afterLegacy != legacy || afterAllocations != allocations {
		t.Fatalf("capture resume did not replay its retained operation (%d -> %d posts, %d -> %d allocations)", posts, afterPosts, allocations, afterAllocations)
	}
	if _, err := os.Stat(filepath.Join(fixture.dir, ".provider.jwt")); err != nil {
		t.Fatalf("capture resume did not install the bound credential: %v", err)
	}
	after, err := os.ReadFile(recordPath)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("capture resume changed the retained operation: %s (%v)", after, err)
	}
}

// A caller that recorded no client for the slot asserts no expectation, so a
// bound operation is presented as this slot's own identity rather than refused
// for differing from nothing.
func TestCaptureSlotWithoutRecordedClientPresentsItsBoundRecord(t *testing.T) {
	fixture := newProviderRegistrationFixture(t)
	bound := providerCaptureBoundCustody(t, fixture)
	clientId, err := providerCaptureLaunchRegister(t, fixture, false, &clientauth.RetainedClientIdentity{})
	if err != nil {
		t.Fatalf("capture slot with no recorded client: %v", err)
	}
	if clientId != bound {
		t.Fatalf("capture slot with no recorded client presented %v, want the bound %v", clientId, bound)
	}
	if _, err := os.Stat(filepath.Join(fixture.dir, ".provider.jwt")); err != nil {
		t.Fatalf("capture slot with no recorded client installed no credential: %v", err)
	}
}

// providerCaptureRecordedCustody runs an ordinary launch through its
// authenticated handoff, leaving the slot with both the durable operation and
// the credential it installed for that client.
func providerCaptureRecordedCustody(t *testing.T, fixture *providerRegistrationFixture) connect.Id {
	t.Helper()
	stop := errors.New("synthetic recorded-custody handoff")
	var bound connect.Id
	if err := fixture.run(t.Context(), true, providerRegistrationHooks{afterAuthenticated: func(_ string, id connect.Id, _ []byte) error {
		bound = id
		return stop
	}}); !errors.Is(err, stop) {
		t.Fatalf("fixture did not complete a recorded registration: %v", err)
	}
	if bound == (connect.Id{}) {
		t.Fatal("fixture completed without binding a client")
	}
	recordPath := filepath.Join(fixture.dir, ".provider.jwt.registration")
	var record providerRegistrationTestRecord
	raw, err := os.ReadFile(recordPath)
	if err != nil || json.Unmarshal(raw, &record) != nil || record.ClientId != bound.String() || record.DeviceId == "" {
		t.Fatalf("fixture record = %s, want a binding to %v (%v)", raw, bound, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.dir, ".provider.jwt")); err != nil {
		t.Fatalf("fixture left no credential: %v", err)
	}
	return bound
}

// providerCaptureCredentialOnlyCustody leaves a slot with its provider key and
// one credential, without the durable operation a completed registration keeps.
func providerCaptureCredentialOnlyCustody(t *testing.T, fixture *providerRegistrationFixture, client connect.Id) {
	t.Helper()
	keyOwner, err := clientauth.OpenProviderClientKey(t.Context(), filepath.Join(fixture.dir, ".provider.key"), clientauth.ProviderClientKeyOptions{AllowCreate: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := keyOwner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := clientauth.WriteToken(filepath.Join(fixture.dir, ".provider.jwt"), providerRegistrationTestToken(t, client.String(), "existing")); err != nil {
		t.Fatal(err)
	}
}

// A capture slot that already holds custody for another client refuses at the
// credential, before the marker or the refresh writes it and before the launch
// can present it. The refusal names the client the profile recorded, never the
// one this slot could have presented.
func TestCaptureSlotHoldingAnotherClientCredentialRefusesBeforeWriting(t *testing.T) {
	recorded := connect.Id{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xf0, 0x01}
	profile := &ProviderWorkCaptureProfile{Providers: []ProviderWorkCaptureOwner{{Slot: "direct", ClientId: [16]byte(recorded)}}}
	heldWithoutRecord := connect.Id{0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x5b, 0x5c, 0x5d, 0x5e, 0x5f, 0x60}

	for _, test := range []struct {
		name    string
		prepare func(t *testing.T, fixture *providerRegistrationFixture) connect.Id
	}{
		{
			name: "credential and durable record",
			prepare: func(t *testing.T, fixture *providerRegistrationFixture) connect.Id {
				return providerCaptureRecordedCustody(t, fixture)
			},
		},
		{
			name: "credential without durable record",
			prepare: func(t *testing.T, fixture *providerRegistrationFixture) connect.Id {
				providerCaptureCredentialOnlyCustody(t, fixture, heldWithoutRecord)
				return heldWithoutRecord
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newProviderRegistrationFixture(t)
			held := test.prepare(t, fixture)
			if held == recorded {
				t.Fatalf("fixture held the recorded client %v", held)
			}
			credentialPath := filepath.Join(fixture.dir, ".provider.jwt")
			markerPath := filepath.Join(fixture.dir, ".provider.jwt.registration.existing")
			credential, err := os.ReadFile(credentialPath)
			if err != nil {
				t.Fatal(err)
			}
			marker, markerErr := os.ReadFile(markerPath)
			posts, legacy, allocations, refreshes := fixture.counts()
			_, err = providerCaptureLaunchRegister(t, fixture, providerSlotCreatePermitted(true, false, captureLaunchIdentityRefusal(profile, nil, "direct")), captureLaunchRetainedIdentity(profile, nil, "direct"))
			var refused *clientauth.RegistrationRefusedError
			if err == nil || !errors.As(err, &refused) || refused.Code != "retained_client_mismatch" {
				after, _ := os.ReadFile(credentialPath)
				afterMarker, _ := os.ReadFile(markerPath)
				afterPosts, afterLegacy, afterAllocations, afterRefreshes := fixture.counts()
				t.Fatalf("capture slot holding %v: err = %v, want retained_client_mismatch (credential changed %v, marker changed %v, posts %d->%d, legacy %d->%d, allocations %d->%d, refreshes %d->%d)", held, err, !bytes.Equal(credential, after), !bytes.Equal(marker, afterMarker), posts, afterPosts, legacy, afterLegacy, allocations, afterAllocations, refreshes, afterRefreshes)
			}
			if refused.ExpectedClientId != recorded.String() || !strings.Contains(err.Error(), recorded.String()) {
				t.Fatalf("refusal %q names %q, want the recorded client %s", err, refused.ExpectedClientId, recorded)
			}
			if strings.Contains(err.Error(), held.String()) {
				t.Fatalf("refusal = %q, want the client this slot could not present left out of the requirement", err)
			}
			afterPosts, afterLegacy, afterAllocations, afterRefreshes := fixture.counts()
			if afterPosts != posts || afterLegacy != legacy || afterAllocations != allocations || afterRefreshes != refreshes {
				t.Fatalf("capture slot reached the registration endpoint (%d->%d posts, %d->%d legacy, %d->%d allocations, %d->%d refreshes)", posts, afterPosts, legacy, afterLegacy, allocations, afterAllocations, refreshes, afterRefreshes)
			}
			after, err := os.ReadFile(credentialPath)
			if err != nil || !bytes.Equal(after, credential) {
				t.Fatalf("capture slot changed the credential it must not present: %v", err)
			}
			afterMarker, afterMarkerErr := os.ReadFile(markerPath)
			if (markerErr == nil) != (afterMarkerErr == nil) || !bytes.Equal(afterMarker, marker) {
				t.Fatalf("capture slot created or modified the existing-client marker: %v -> %v", markerErr, afterMarkerErr)
			}
		})
	}
}

// The same custody is a legitimate capture reuse when the profile recorded the
// client it belongs to: the refresh path returns that client.
func TestCaptureSlotHoldingTheRecordedClientCredentialReuses(t *testing.T) {
	fixture := newProviderRegistrationFixture(t)
	held := providerCaptureRecordedCustody(t, fixture)
	profile := &ProviderWorkCaptureProfile{Providers: []ProviderWorkCaptureOwner{{Slot: "direct", ClientId: [16]byte(held)}}}
	_, _, _, refreshes := fixture.counts()
	clientId, err := providerCaptureLaunchRegister(t, fixture, providerSlotCreatePermitted(true, false, captureLaunchIdentityRefusal(profile, nil, "direct")), captureLaunchRetainedIdentity(profile, nil, "direct"))
	if err != nil {
		t.Fatalf("capture slot holding its recorded client: %v", err)
	}
	if clientId != held {
		t.Fatalf("capture launch presented %v, want the recorded %v", clientId, held)
	}
	_, _, _, afterRefreshes := fixture.counts()
	if afterRefreshes <= refreshes || fixture.refreshedClients()[held.String()] < 1 {
		t.Fatalf("capture launch did not reuse the credential through its refresh path: refreshes %d -> %d, clients %v", refreshes, afterRefreshes, fixture.refreshedClients())
	}
	for client := range fixture.refreshedClients() {
		if client != held.String() {
			t.Fatalf("capture launch refreshed %s, want only the recorded %s", client, held)
		}
	}
}

// A caller that recorded no client asserts no expectation: the marker write and
// the refresh proceed and the credential's own client comes back.
func TestCaptureSlotWithoutRecordedClientReusesTheCredential(t *testing.T) {
	held := connect.Id{0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x5b, 0x5c, 0x5d, 0x5e, 0x5f, 0x60}
	fixture := newProviderRegistrationFixture(t)
	providerCaptureCredentialOnlyCustody(t, fixture, held)
	markerPath := filepath.Join(fixture.dir, ".provider.jwt.registration.existing")
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture started with an existing-client marker: %v", err)
	}
	clientId, err := providerCaptureLaunchRegister(t, fixture, false, &clientauth.RetainedClientIdentity{})
	if err != nil {
		t.Fatalf("capture slot with no recorded client: %v", err)
	}
	if clientId != held {
		t.Fatalf("capture slot with no recorded client presented %v, want %v", clientId, held)
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("capture slot with no recorded client did not observe existing custody: %v", err)
	}
}
