package miner

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/urnetwork/connect"
)

// h3_mode.go ports the 3.23-fix v33 provider's h3 control key (provider/
// h3_mode.go). The key is the ONE user-facing limit on how many H3 (QUIC)
// transport connections are active:
//
//	off     H1 only. No QUIC and no dns carrier is ever dialed.
//	direct  the default. The direct (native) identity runs every upstream
//	        carrier as an automatic fallback: H1 is the primary, and H3, H3 over
//	        dns and the dns pump are started in that order only while nothing
//	        better is connected, and stand down once something better is. Idle
//	        they cost nothing. Proxied identities stay on H1.
//	on      direct, plus the opt-in auxiliary: H3 runs beside H1 on the direct
//	        identity instead of waiting behind it. This is what URNETWORK_H3 has
//	        always meant, so persisted values keep their behavior.
//	<N>     direct, plus the same automatic fallbacks for the N best proxies
//	        (see h3_proxy_set.go). Their QUIC leaves through the proxy's SOCKS5
//	        UDP relay and never from the host; a proxy that cannot relay UDP
//	        stays on H1 (see proxy_udp_check.go).
//	all     direct, plus the automatic fallbacks for every proxy.
//
// The setting is read when an identity builds its platform transport (see
// platformTransportSettingsFor), so a change reaches an identity at its next
// (re)start; a change of setting relaunches the identities it affects (see
// h3_mode_apply.go). The pinned full-bars/connect carries the transport mode
// preferences and the H3 packet conn factory, but not the fork's process-wide
// H3 gate (connect.SetH3Enabled / PlatformTransportSettings.EnableH3), so
// "beside H1" is expressed the way the preferences always did: H3 at H1's own
// tier (see h3ModePreferences).
type h3Mode int

const (
	h3ModeOff h3Mode = iota
	h3ModeDirect
	h3ModeOn
	h3ModeTop
	h3ModeAll
)

const (
	h3ModeOffName    = "off"
	h3ModeDirectName = "direct"
	h3ModeOnName     = "on"
	h3ModeAllName    = "all"
)

// h3Setting is a parsed h3 value: the mode, plus the proxy count for h3ModeTop.
type h3Setting struct {
	Mode h3Mode
	Cap  int
}

func (s h3Setting) String() string {
	switch s.Mode {
	case h3ModeOff:
		return h3ModeOffName
	case h3ModeOn:
		return h3ModeOnName
	case h3ModeTop:
		return strconv.Itoa(s.Cap)
	case h3ModeAll:
		return h3ModeAllName
	default:
		return h3ModeDirectName
	}
}

// proxiesMayUseTransports is whether any proxied identity can be given the
// fallback transports under this setting.
func (s h3Setting) proxiesMayUseTransports() bool {
	return s.Mode == h3ModeTop || s.Mode == h3ModeAll
}

// directPolicy is the mode as far as the direct identity is concerned: a count
// and all both give it the plain fallbacks, the same as direct.
func (s h3Setting) directPolicy() h3Mode {
	if s.proxiesMayUseTransports() {
		return h3ModeDirect
	}
	return s.Mode
}

// parseH3Mode parses a control value. An empty value is the default.
func parseH3Mode(value string) (h3Setting, error) {
	v := strings.ToLower(strings.TrimSpace(value))
	switch v {
	case "":
		return h3Setting{Mode: h3ModeDirect}, nil
	case h3ModeOffName, "0":
		return h3Setting{Mode: h3ModeOff}, nil
	case h3ModeDirectName:
		return h3Setting{Mode: h3ModeDirect}, nil
	case h3ModeOnName:
		return h3Setting{Mode: h3ModeOn}, nil
	case h3ModeAllName:
		return h3Setting{Mode: h3ModeAll}, nil
	}
	if n, err := strconv.Atoi(v); err == nil && 0 < n && strconv.Itoa(n) == v {
		return h3Setting{Mode: h3ModeTop, Cap: n}, nil
	}
	return h3Setting{Mode: h3ModeDirect}, fmt.Errorf("h3: must be off, direct, on, a positive proxy count, or all (got %q)", value)
}

// resolveH3Mode is the setting in force: the h3 control key when it is set and
// valid, otherwise the startup default. The startup default keeps the existing
// opt-ins: URNETWORK_H3 and the direct-only default (see defaultH3Setting)
// select the beside-H1 auxiliary, and everything else gets the automatic
// fallbacks.
func resolveH3Mode(state *controlState, resolution int32) h3Setting {
	if state != nil {
		if v, ok := state.get("h3"); ok {
			if setting, err := parseH3Mode(v); err == nil {
				return setting
			}
		}
	}
	if defaultH3Setting(state, resolution) {
		return h3Setting{Mode: h3ModeOn}
	}
	return h3Setting{Mode: h3ModeDirect}
}

// h3ModePreferences is the transport mode policy of one identity under a
// setting. Lower is better; a mode runs only while nothing better is connected,
// except that equal tiers run side by side. A proxied identity gets the fallbacks
// only when the setting admits it (proxyEligible, see h3ProxyEligible).
//
// h3ModeOn is the beside-H1 auxiliary: H3 shares H1's tier, which is exactly
// what the pinned connect reads as "run these together". The fork expresses the
// same state with PlatformTransportSettings.EnableH3, which the pinned
// full-bars/connect does not carry; the tier is the portable spelling.
func h3ModePreferences(setting h3Setting, direct bool, proxyEligible bool) map[connect.TransportMode]int {
	if setting.Mode == h3ModeOff || (!direct && !proxyEligible) {
		return map[connect.TransportMode]int{connect.TransportModeH1: 1}
	}
	preferences := connect.DefaultTransportModePreferences()
	if direct && setting.Mode == h3ModeOn {
		// the auxiliary: H3 shares H1's tier
		preferences[connect.TransportModeH3] = preferences[connect.TransportModeH1]
	}
	return preferences
}
