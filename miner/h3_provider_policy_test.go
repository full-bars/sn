package miner

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// Tests for the h3 mode reaching the provider carrier. The miner's provider
// slots are sdk.DeviceLocal devices, so the mode travels as the sdk's Auto
// policy (see h3_provider_policy.go) rather than as a connect
// PlatformTransportSettings the miner builds itself.

// h3PolicyPreferences reads a provider policy back with the production parser
// (h3ProviderPolicyOf), so a test asserts the policy the engine will install
// rather than the sdk's struct layout, and exercises the same read the
// constructor validates.
func h3PolicyPreferences(t *testing.T, settings *sdk.TransportSettings) map[connect.TransportMode]int {
	t.Helper()
	policy, err := h3ProviderPolicyOf(settings)
	if err != nil {
		t.Fatalf("read provider carrier policy: %v", err)
	}
	if policy.Mode != sdk.TransportModeAuto {
		t.Fatalf("mode = %q, want %q: the h3 modes are an Auto policy", policy.Mode, sdk.TransportModeAuto)
	}
	return policy.Priorities
}

// The policy per mode and per identity role. The direct identity is admitted by
// every mode except off; a proxy only when the setting admits proxies, which is
// the split the provider package makes with snH3Eligible.
func TestH3ProviderTransportSettingsPerMode(t *testing.T) {
	h3ResetTestState(t)
	h1Only := map[connect.TransportMode]int{connect.TransportModeH1: 1}
	auxiliary := connect.DefaultTransportModePreferences()
	auxiliary[connect.TransportModeH3] = auxiliary[connect.TransportModeH1]

	cases := []struct {
		name     string
		setting  h3Setting
		direct   bool
		eligible bool
		want     map[connect.TransportMode]int
	}{
		{"off direct", h3Setting{Mode: h3ModeOff}, true, false, h1Only},
		{"off proxy", h3Setting{Mode: h3ModeOff}, false, true, h1Only},
		{"direct direct", h3Setting{Mode: h3ModeDirect}, true, false, connect.DefaultTransportModePreferences()},
		{"direct proxy", h3Setting{Mode: h3ModeDirect}, false, false, h1Only},
		{"on direct", h3Setting{Mode: h3ModeOn}, true, false, auxiliary},
		{"on proxy", h3Setting{Mode: h3ModeOn}, false, false, h1Only},
		{"all proxy eligible", h3Setting{Mode: h3ModeAll}, false, true, connect.DefaultTransportModePreferences()},
		{"all proxy ineligible", h3Setting{Mode: h3ModeAll}, false, false, h1Only},
		{"top proxy eligible", h3Setting{Mode: h3ModeTop, Cap: 2}, false, true, connect.DefaultTransportModePreferences()},
		{"top proxy ineligible", h3Setting{Mode: h3ModeTop, Cap: 2}, false, false, h1Only},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := h3PolicyPreferences(t, h3ProviderTransportSettings(c.setting, c.direct, c.eligible))
			if !maps.Equal(got, c.want) {
				t.Fatalf("policy = %v, want %v", got, c.want)
			}
		})
	}
}

// The mode in force reaches a slot's policy, and a change of the control key
// moves it: the same call the provide run makes when a slot builds its device
// (h3SlotTransportSettings), driven through the control path.
func TestH3ModeChangeRebuildsSlotPolicy(t *testing.T) {
	h3ResetTestState(t)
	candidates := testH3Proxies(2)
	h3TestUDPAdmitsAll(t, candidates)
	proxy := candidates[0]

	h1Only := map[connect.TransportMode]int{connect.TransportModeH1: 1}
	assertPolicy := func(t *testing.T, label string, settings *sdk.TransportSettings, want map[connect.TransportMode]int) {
		t.Helper()
		if got := h3PolicyPreferences(t, settings); !maps.Equal(got, want) {
			t.Fatalf("%s policy = %v, want %v", label, got, want)
		}
	}

	// `set h3 <value>` persists the value into the shared control state (which
	// resolveH3Mode reads) and applies it live through applyH3Mode; a slot that
	// starts afterwards reads the new mode when it builds its policy.
	state := newControlState()
	globalControlState = state
	set := func(t *testing.T, value string) h3Setting {
		t.Helper()
		if err := state.set("h3", value); err != nil {
			t.Fatalf("set h3 %s: %v", value, err)
		}
		setting, err := parseH3Mode(value)
		if err != nil {
			t.Fatalf("parse h3 %s: %v", value, err)
		}
		return setting
	}

	// off: neither identity may dial a QUIC carrier.
	if _, relaunch := applyH3Mode(set(t, "off")); relaunch {
		t.Fatal("the startup application must not relaunch anything")
	}
	assertPolicy(t, "off direct", h3SlotTransportSettings(nil), h1Only)
	assertPolicy(t, "off proxy", h3SlotTransportSettings(proxy), h1Only)

	// direct: the direct identity gets the automatic fallbacks, the proxy stays
	// on H1. The direct identity's policy moved, so the running one is relaunched.
	if _, relaunch := applyH3Mode(set(t, "direct")); !relaunch {
		t.Fatal("direct mode did not report the direct identity for relaunch")
	}
	assertPolicy(t, "direct direct", h3SlotTransportSettings(nil), connect.DefaultTransportModePreferences())
	assertPolicy(t, "direct proxy", h3SlotTransportSettings(proxy), h1Only)

	// all: a proxy that joins the set is rebuilt with H3 by the relaunch the
	// change schedules (see h3ProxiesToRelaunch): its launch row says it was
	// built without the fallbacks, and the new mode admits every proxy.
	setting := set(t, "all")
	applyH3Mode(setting)
	relaunch := h3ProxiesToRelaunch(setting)
	if len(relaunch) != 1 || relaunch[0] != proxy.Key() {
		t.Fatalf("proxies to relaunch = %v, want [%s]", relaunch, proxy.Key())
	}
	assertPolicy(t, "all proxy", h3SlotTransportSettings(proxy), connect.DefaultTransportModePreferences())
}

// h3ProviderDeviceFixture builds a real provider device through the production
// constructor, with a loopback platform and no egress beyond it.
func h3ProviderDeviceFixture(t *testing.T, policy *sdk.TransportSettings) *sdk.DeviceLocal {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	strategy := connect.DefaultClientStrategySettings()
	strategy.EnableResilient = false
	strategy.TlsConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	strategy.DialContextSettings = &connect.DialContextSettings{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != server.Listener.Addr().String() {
			return nil, errors.New("h3 policy fixture refuses nonlocal egress")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	space := sdk.NewNetworkSpaceWithUrls(ctx, server.URL, "wss"+strings.TrimPrefix(server.URL, "https"), strategy)
	settings := ProviderDeviceSettings([32]byte{})
	settings.ProvideExtenderEnabled = false
	// A loopback platform names its own pump ingress, as the other loopback
	// provider fixtures do: the sdk default pump is a production host.
	settings.DnsPumpHost = server.Listener.Addr().(*net.TCPAddr).IP.String()
	settings.KeyMaterial = sdk.NewDeviceLocalKeyMaterial(make([]byte, 32), nil, nil)
	clientId := connect.Id{1}
	device, err := newProviderDeviceLocal(ctx, space, strategy, providerRegistrationTestToken(t, clientId.String(), "h3-policy"), "h3 policy fixture", settings, nil, "direct", clientId, policy)
	if err != nil {
		cancel()
		space.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		join, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		if err := device.CloseAndWait(join); err != nil {
			t.Errorf("h3 policy fixture device did not close: %v", err)
		}
		space.Close()
	})
	return device
}

// The device the constructor returns carries the policy it was handed, not the
// sdk's default. The constructor sets the policy after the sdk built the
// carrier and reads it back (h3ApplyProviderTransportSettings), so this also
// covers the read-back end to end: a policy the sdk refused would fail the
// constructor here instead of being returned silently.
//
// It is not proof that the carrier swapped: the sdk delivers the policy by an
// asynchronous make-before-break replacement that has no completion signal, so
// a swap that times out still reads back equal (see h3_provider_policy.go).
func TestH3ProviderDeviceCarriesSlotPolicy(t *testing.T) {
	off := h3ProviderDeviceFixture(t, h3ProviderTransportSettings(h3Setting{Mode: h3ModeOff}, true, false))
	if got := h3PolicyPreferences(t, off.GetProviderTransportSettings()); !maps.Equal(got, map[connect.TransportMode]int{connect.TransportModeH1: 1}) {
		t.Fatalf("off device policy = %v, want H1 only", got)
	}

	all := h3ProviderDeviceFixture(t, h3ProviderTransportSettings(h3Setting{Mode: h3ModeAll}, false, true))
	got := h3PolicyPreferences(t, all.GetProviderTransportSettings())
	want := connect.DefaultTransportModePreferences()
	if !maps.Equal(got, want) {
		t.Fatalf("all device policy = %v, want %v", got, want)
	}
	if got[connect.TransportModeH3] <= got[connect.TransportModeH1] {
		t.Fatalf("all device policy = %v: H3 must be an automatic fallback, not beside H1", got)
	}
}

// The read-back compares the policy the engine reads, not the sdk's struct, so a
// divergence is caught even though the sdk sorts its stored copy and may rewrite
// a legacy policy. This is the comparison that turns a refused policy into a
// construction error; a refusal cannot be forced through a real device (the sdk
// stores a refused value only when a local state is present and auto-save is on,
// and the miner enables neither), so the comparison is exercised directly.
func TestH3ProviderPolicyReadBackDetectsDivergence(t *testing.T) {
	read := func(setting h3Setting) h3ProviderPolicy {
		t.Helper()
		policy, err := h3ProviderPolicyOf(h3ProviderTransportSettings(setting, true, false))
		if err != nil {
			t.Fatalf("read %s policy: %v", setting, err)
		}
		return policy
	}
	off := read(h3Setting{Mode: h3ModeOff})
	direct := read(h3Setting{Mode: h3ModeDirect})

	if !h3ProviderPolicyEqual(off, off) {
		t.Fatalf("read-back did not treat %v as itself", off.Priorities)
	}
	if h3ProviderPolicyEqual(off, direct) {
		t.Fatalf("read-back treated off %v and direct %v as the same policy", off.Priorities, direct.Priorities)
	}

	// The stored copy the sdk hands back is canonical: sorted by priority and
	// then mode order. Equal policies must compare equal regardless.
	stored := &sdk.TransportSettings{Mode: sdk.TransportModeAuto, AutoModePriorities: sdk.NewTransportModePriorityList()}
	stored.AutoModePriorities.Add(&sdk.TransportModePriority{Mode: sdk.TransportModeDnsPump, Priority: 4})
	stored.AutoModePriorities.Add(&sdk.TransportModePriority{Mode: sdk.TransportModeH1, Priority: 1})
	stored.AutoModePriorities.Add(&sdk.TransportModePriority{Mode: sdk.TransportModeDns, Priority: 3})
	stored.AutoModePriorities.Add(&sdk.TransportModePriority{Mode: sdk.TransportModeH3, Priority: 2})
	canonical, err := h3ProviderPolicyOf(stored)
	if err != nil {
		t.Fatalf("read canonical policy: %v", err)
	}
	if !h3ProviderPolicyEqual(canonical, direct) {
		t.Fatalf("read-back rejected a reordered copy of the direct policy: %v", canonical.Priorities)
	}

	// A different mode is a different policy even with the same priorities.
	if h3ProviderPolicyEqual(h3ProviderPolicy{Mode: sdk.TransportModeH1, Priorities: off.Priorities}, off) {
		t.Fatal("read-back ignored the sdk mode")
	}
}
