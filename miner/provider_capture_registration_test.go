package miner

import (
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
