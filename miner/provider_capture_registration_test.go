package miner

import (
	"context"
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
// launch computed.
func providerCaptureLaunchRegister(t *testing.T, fixture *providerRegistrationFixture, allowCreate bool) (connect.Id, error) {
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
	_, clientId, err := keyOwner.LoadOrRegisterClientJwt(ctx, api, filepath.Join(fixture.dir, "jwt"), filepath.Join(fixture.dir, ".provider.jwt"), "direct", allowCreate)
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
	_, err := providerCaptureLaunchRegister(t, capture, providerSlotCreatePermitted(true, false, refusal))
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
	allocated, err := providerCaptureLaunchRegister(t, control, providerSlotCreatePermitted(true, false, nil))
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
