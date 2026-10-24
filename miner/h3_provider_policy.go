package miner

import (
	"errors"
	"fmt"
	"maps"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// h3_provider_policy.go carries the h3 mode into the provider device's carrier
// policy.
//
// The provider package writes connect.PlatformTransportSettings.ModePreferences
// directly (provider/h3_mode.go, applyH3ModeToSettings). The miner does not
// build that struct: a provider slot is an sdk.DeviceLocal, and the pinned sdk
// builds the provider carrier from its own TransportSettings and turns that into
// the ModePreferences the engine reads. So the same decision travels as an sdk
// Auto policy: h3ModePreferences is the ONE policy, and this file only re-spells
// it in the sdk's vocabulary.
//
// Delivery is NOT at construction. The pinned sdk has no DeviceLocalSettings
// field for the provider carrier policy, and sdk.NewDeviceLocal builds the
// provider, and its carrier, on DefaultProviderTransportSettings()
// (Auto {H1:1, H3:2, dns:3, pump:4}) before it returns. The earliest seam is a
// policy set immediately AFTER construction, which the sdk delivers by
// make-before-break replacement (SetProviderTransportSettings ->
// setLocalCatalogPreference -> SetTransportPolicy ->
// requestPlatformTransportMigration): in the normal case the default-policy
// carrier keeps dialing and carrying until the replacement connects, and only
// then is it closed. (The sdk breaks first only where the memory budget cannot
// hold both generations, which its H3-family path reserves for an H3-to-H3
// policy change.)
//
// So there is a window on the sdk default policy, from construction until the
// replacement connects. What can happen in it:
//   - The provider carrier can dial H3 on the default Auto policy. H3 is
//     priority 2 behind H1 (priority 1), so it is a fallback: it is dialed when
//     H1 is slow or failing, not instead of H1. `off` cannot prevent that dial
//     inside the window, only once the policy is in force.
//   - The caller's provide-mode declaration lands in it. SetProvideControlMode
//     runs right after construction and setProvideMode installs the new auth
//     (provide intent) on the CURRENT generation, which is still the
//     default-policy carrier, so the window can carry provide intent as well as
//     traffic. The declaration is not held back for the swap: the swap has no
//     completion signal (below), and a provider that never declares its mode
//     serves nothing.
//   - A policy equal to the sdk default has no window at all: the setter finds
//     the stored policy equal, stores nothing and starts no migration. That is
//     the `direct` mode (and every mode for the swarm run, which passes nil).
//
// The window cannot be closed or waited out from the miner. The sdk exposes no
// migration-completion signal: GetProviderTransportStatus is structural
// eligibility derived from the STORED settings (transport_status.go), not the
// live generation, and there is no exported policy version, migration state or
// carrier policy readout. Waiting on it would be waiting on the value just
// written, so nothing here waits.
//
// A failed swap is not observable either. If the replacement does not connect
// within the sdk's migrateConnectTimeout it is closed and the default-policy
// carrier is kept, while GetProviderTransportSettings reports the new policy. A
// second, independent attempt follows in the normal provide run:
// SetProvideControlMode(Always) flips the provide mode to public right after
// construction, which bumps the provider's policy version and requests another
// migration, and that one rebuilds from the stored policy. If both attempts
// time out, the slot keeps serving the sdk default until it is relaunched --
// `set h3 off` did not take effect on that slot, and nothing in the miner can
// see it. The read-back below cannot see it either (it reads the stored value);
// it exists so a policy the sdk REFUSES to store fails loudly instead of
// leaving the default in force.

// h3TransportModeName is the sdk spelling of a connect transport mode, empty
// for a mode the sdk has no name for.
func h3TransportModeName(mode connect.TransportMode) string {
	switch mode {
	case connect.TransportModeH1:
		return sdk.TransportModeH1
	case connect.TransportModeH3:
		return sdk.TransportModeH3
	case connect.TransportModeH3Dns:
		return sdk.TransportModeDns
	case connect.TransportModeH3DnsPump:
		return sdk.TransportModeDnsPump
	}
	return ""
}

// h3ConnectTransportMode is the inverse of h3TransportModeName: the connect mode
// an sdk policy entry names, false for a name this build does not know.
func h3ConnectTransportMode(name string) (connect.TransportMode, bool) {
	switch name {
	case sdk.TransportModeH1:
		return connect.TransportModeH1, true
	case sdk.TransportModeH3:
		return connect.TransportModeH3, true
	case sdk.TransportModeDns:
		return connect.TransportModeH3Dns, true
	case sdk.TransportModeDnsPump:
		return connect.TransportModeH3DnsPump, true
	}
	return connect.TransportModeNone, false
}

// h3ProviderTransportSettings is the provider carrier policy of one identity
// under a setting, in the form the pinned sdk accepts.
//
// Lower is better and equal tiers run side by side, exactly as the engine reads
// the preferences (see h3ModePreferences). The modes are written in the engine's
// order (H1, H3, H3 over dns, the dns pump) so the policy is stable to read and
// to compare; the sdk sorts by priority and then by its own mode order anyway.
//
// The beside-H1 auxiliary survives the sdk's normalization: the sdk rewrites a
// policy whose entries are all the pre-H1-primary defaults ({H1:1, H3:1, dns:2,
// dnspump:3}) and leaves any other policy alone. h3ModePreferences' auxiliary is
// {H1:1, H3:1, dns:3, dnspump:4}, so the tie is preserved (H1-only, which the
// rewrite leaves at 1, is unaffected either way).
func h3ProviderTransportSettings(setting h3Setting, direct bool, proxyEligible bool) *sdk.TransportSettings {
	preferences := h3ModePreferences(setting, direct, proxyEligible)
	priorities := sdk.NewTransportModePriorityList()
	for _, mode := range []connect.TransportMode{
		connect.TransportModeH1,
		connect.TransportModeH3,
		connect.TransportModeH3Dns,
		connect.TransportModeH3DnsPump,
	} {
		priority, ok := preferences[mode]
		if !ok {
			continue
		}
		name := h3TransportModeName(mode)
		if name == "" {
			continue
		}
		priorities.Add(&sdk.TransportModePriority{Mode: name, Priority: priority})
	}
	return &sdk.TransportSettings{Mode: sdk.TransportModeAuto, AutoModePriorities: priorities}
}

// h3ProviderPolicy is a provider carrier policy in the form the engine reads it:
// the sdk mode plus the Auto mode->priority map. Comparing this rather than the
// sdk struct keeps the read-back independent of the sdk's ordering and of its
// normalization rewrite.
type h3ProviderPolicy struct {
	Mode       string
	Priorities map[connect.TransportMode]int
}

// h3ProviderPolicyOf reads an sdk provider policy as the engine reads it. A
// policy with no priorities, a nil entry, or a mode name this build does not
// know is an error: it cannot be compared, and it is not a policy the miner
// writes.
func h3ProviderPolicyOf(settings *sdk.TransportSettings) (h3ProviderPolicy, error) {
	if settings == nil {
		return h3ProviderPolicy{}, errors.New("provider carrier policy is nil")
	}
	policy := h3ProviderPolicy{
		Mode:       settings.Mode,
		Priorities: map[connect.TransportMode]int{},
	}
	if settings.AutoModePriorities == nil {
		return policy, fmt.Errorf("provider carrier policy %q carries no auto mode priorities", settings.Mode)
	}
	for index := 0; index < settings.AutoModePriorities.Len(); index += 1 {
		item := settings.AutoModePriorities.Get(index)
		if item == nil {
			return policy, fmt.Errorf("provider carrier policy %q has a nil priority at %d", settings.Mode, index)
		}
		mode, ok := h3ConnectTransportMode(item.Mode)
		if !ok {
			return policy, fmt.Errorf("provider carrier policy %q names unknown transport mode %q", settings.Mode, item.Mode)
		}
		policy.Priorities[mode] = item.Priority
	}
	return policy, nil
}

// h3ProviderPolicyEqual is the read-back comparison: the same sdk mode and the
// same mode->priority map. Order does not matter because the sdk sorts its
// stored copy by priority and then by mode order.
func h3ProviderPolicyEqual(a h3ProviderPolicy, b h3ProviderPolicy) bool {
	return a.Mode == b.Mode && maps.Equal(a.Priorities, b.Priorities)
}

// h3ApplyProviderTransportSettings hands `policy` to the device and reads it
// back.
//
// The sdk setter returns nothing and swallows a refused value: it goes through
// setLocalCatalogPreference, which reports a rejection by storing nothing. A
// refused policy would leave the previous one (at construction, the sdk default)
// in force while the caller believes the mode applied, so a mismatch is returned
// and the caller fails the slot. Nothing is repaired here: re-setting the same
// value is a no-op in the sdk (it compares before applying), so a retry could
// only pretend to fix it.
//
// The read-back covers the stored value, not delivery. The swap it starts is
// asynchronous and has no completion signal, so a swap that times out still
// reads back equal (see the file comment).
func h3ApplyProviderTransportSettings(device *sdk.DeviceLocal, policy *sdk.TransportSettings) error {
	want, err := h3ProviderPolicyOf(policy)
	if err != nil {
		return fmt.Errorf("provider carrier policy for this slot: %w", err)
	}
	device.SetProviderTransportSettings(policy)
	got, err := h3ProviderPolicyOf(device.GetProviderTransportSettings())
	if err != nil {
		return fmt.Errorf("read back the provider carrier policy: %w", err)
	}
	if !h3ProviderPolicyEqual(got, want) {
		return fmt.Errorf(
			"the provider kept carrier policy %q %v instead of %q %v: the sdk refused the h3 policy, so this slot would run the previous policy until it is relaunched",
			got.Mode, got.Priorities, want.Mode, want.Priorities,
		)
	}
	return nil
}

// h3SlotTransportSettings is the provider carrier policy for one slot that is
// starting. It reads the mode in force, records the launch row a later change
// compares against (see h3ProxiesToRelaunch), and returns the policy the device
// is given right after it is built (see newProviderDeviceLocal).
//
// The row and the policy are one decision on purpose: two reads could straddle
// a control change, leaving the relaunch pass either rebuilding an identity that
// was already built with the new mode or skipping one that was not.
func h3SlotTransportSettings(proxySettings *connect.ProxySettings) *sdk.TransportSettings {
	setting := resolveH3Mode(globalControlState, currentProxyResolution())
	eligible := noteH3Launch(proxySettings, setting)
	return h3ProviderTransportSettings(setting, proxySettings == nil, eligible)
}
