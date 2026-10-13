package provider

// Dual-stack platform transport for the direct identity.
//
// A direct identity normally holds one family-agnostic platform transport, so
// the platform sees whichever family the dial happened to pick. On a host with
// a real global IPv6 address the identity instead runs connect's family group:
// one transport pinned to IPv4, one pinned to IPv6, and a parked family-agnostic
// standby that stands in only if neither pin connects. Holding both families
// lets the platform categorize the provider as dual-stack.
//
// Only the direct identity ever gets the group. A proxied identity reaches the
// platform through its proxy, and a pinned transport dials directly from the
// host, so a proxied identity stays on the single legacy transport (H1, and
// whatever family its proxy exits from). H3 stays on for the direct group: each
// pinned transport opens a UDP socket of its own family (newH3PacketConnFactory).
//
// URNETWORK_DUALSTACK=0 turns the feature off.

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// providerPlatformTransport is the slice of a platform transport that
// provideWithProxy's consumers need. Both *connect.PlatformTransport and
// *connect.FamilyPlatformTransportGroup satisfy it.
type providerPlatformTransport interface {
	IsConnected() bool
	ConnectedNotify() <-chan struct{}
	SetAuth(auth *connect.ClientAuth)
	Kick()
	Close()
}

var (
	_ providerPlatformTransport = (*connect.PlatformTransport)(nil)
	_ providerPlatformTransport = (*connect.FamilyPlatformTransportGroup)(nil)
)

// familyTransportKillEnv turns the dual-stack group off when set to 0, off,
// false or no. Anything else, including unset, leaves it on. It is read on every
// direct-identity launch, so a restart or hot restart applies a change.
const familyTransportKillEnv = "URNETWORK_DUALSTACK"

func familyTransportsKilled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(familyTransportKillEnv))) {
	case "0", "off", "false", "no":
		return true
	}
	return false
}

// familyServiceUrl is the family-pinned form of a platform service url: the
// first host label gets a -v4 or -v6 suffix (connect becomes connect-v4). It
// returns "" when no family name can exist: an IP literal, a single-label host,
// a label that already carries a family suffix, an unparsable url, or a version
// other than 4 or 6.
func familyServiceUrl(serviceUrl string, ipVersion int) string {
	if ipVersion != 4 && ipVersion != 6 {
		return ""
	}
	suffix := "-v4"
	if ipVersion == 6 {
		suffix = "-v6"
	}
	scheme := ""
	rest := serviceUrl
	if i := strings.Index(serviceUrl, "://"); 0 <= i {
		scheme = serviceUrl[:i+3]
		rest = serviceUrl[i+3:]
	}
	if rest == "" {
		return ""
	}
	hostPort := rest
	tail := ""
	if i := strings.IndexAny(rest, "/?#"); 0 <= i {
		hostPort = rest[:i]
		tail = rest[i:]
	}
	if strings.ContainsAny(hostPort, "@ ") {
		return ""
	}
	host, port := hostPort, ""
	if strings.HasPrefix(hostPort, "[") {
		// a bracketed host is an IPv6 literal
		return ""
	}
	if i := strings.LastIndex(hostPort, ":"); 0 <= i {
		host, port = hostPort[:i], hostPort[i:]
	}
	if host == "" || net.ParseIP(host) != nil {
		return ""
	}
	label, domain, ok := strings.Cut(host, ".")
	if !ok || label == "" || domain == "" {
		return ""
	}
	if strings.HasSuffix(label, "-v4") || strings.HasSuffix(label, "-v6") {
		return ""
	}
	return scheme + label + suffix + "." + domain + port + tail
}

// isUsableGlobalIpv6 is true for an address a platform can reach a provider by:
// global unicast, and none of the private, link-local, documentation, 6to4,
// Teredo, NAT64, v4-mapped or v4-compatible ranges. connect's own probe accepts
// a ULA such as Tailscale's fd7a::/48, so sn applies this stricter test to the
// source address the kernel would pick.
func isUsableGlobalIpv6(ip net.IP) bool {
	if ip == nil || ip.To4() != nil || ip.To16() == nil {
		return false
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, cidr := range unusableIpv6Ranges {
		if cidr.Contains(ip) {
			return false
		}
	}
	return true
}

var unusableIpv6Ranges = func() []*net.IPNet {
	ranges := []*net.IPNet{}
	for _, s := range []string{
		"2001:db8::/32",  // documentation
		"2002::/16",      // 6to4
		"2001::/32",      // Teredo
		"64:ff9b::/96",   // NAT64
		"64:ff9b:1::/48", // local-use NAT64
		"::ffff:0:0/96",  // v4-mapped
		"::/96",          // v4-compatible
	} {
		_, cidr, err := net.ParseCIDR(s)
		if err != nil {
			panic(err)
		}
		ranges = append(ranges, cidr)
	}
	return ranges
}()

// hostIpv6Probe is a seam for tests.
var hostIpv6Probe = probeHostIpv6Usable

// probeHostIpv6Usable asks the kernel, without sending a packet, whether this
// host has a global IPv6 source address and a route: a connected UDP socket does
// route and source-address selection only. It answers "can the kernel source a
// global v6 address", not "does v6 egress work".
func probeHostIpv6Usable() (bool, string) {
	return probeIpv6With(
		connect.FamilySupported,
		func() (net.IP, error) {
			conn, err := net.DialUDP("udp6", nil, &net.UDPAddr{IP: net.ParseIP("2606:4700:4700::1111"), Port: 443})
			if err != nil {
				return nil, err
			}
			defer conn.Close()
			return conn.LocalAddr().(*net.UDPAddr).IP, nil
		},
	)
}

func probeIpv6With(familySupported func(int) bool, localSource func() (net.IP, error)) (bool, string) {
	if !familySupported(6) {
		return false, "no-global-v6-address"
	}
	source, err := localSource()
	if err != nil {
		return false, "no-v6-route"
	}
	if !isUsableGlobalIpv6(source) {
		return false, "v6-source-not-global"
	}
	return true, "ok"
}

// familyTransportHostAllowed limits v1 to the one platform host whose -v4 and
// -v6 names were verified live. A custom --connect_url or test backend stays on
// the legacy transport until its family names are verified.
func familyTransportHostAllowed(connectUrl string) bool {
	rest := connectUrl
	if i := strings.Index(rest, "://"); 0 <= i {
		rest = rest[i+3:]
	}
	if i := strings.IndexAny(rest, "/?#:"); 0 <= i {
		rest = rest[:i]
	}
	return strings.EqualFold(rest, "connect.bringyour.com")
}

// useFamilyTransports decides, for one identity launch, whether to build the
// dual-stack group. The checks run in a fixed order so the reason is
// deterministic.
func useFamilyTransports(proxySettings *connect.ProxySettings, isNative bool, connectUrl string) (bool, string) {
	if familyTransportsKilled() {
		return false, "kill-switch"
	}
	if proxySettings != nil {
		return false, "proxied-identity"
	}
	if !isNative {
		return false, "not-native"
	}
	if familyServiceUrl(connectUrl, 4) == "" || familyServiceUrl(connectUrl, 6) == "" {
		return false, "no-family-urls"
	}
	if !familyTransportHostAllowed(connectUrl) {
		return false, "connect-host-not-allowlisted"
	}
	if ok, why := hostIpv6Probe(); !ok {
		return false, why
	}
	return true, "dualstack"
}

// newProviderPlatformTransport builds the identity's platform transport: the
// family group when useFamily, else exactly the single transport the provider
// has always built. Exactly one of legacy and group is non-nil, and transport
// is the matching non-nil interface value (never a typed nil).
func newProviderPlatformTransport(
	ctx context.Context,
	clientStrategySettings *connect.ClientStrategySettings,
	clientStrategy *connect.ClientStrategy,
	routeManager *connect.RouteManager,
	connectUrl string,
	auth *connect.ClientAuth,
	settings *connect.PlatformTransportSettings,
	useFamily bool,
) (transport providerPlatformTransport, legacy *connect.PlatformTransport, group *connect.FamilyPlatformTransportGroup) {
	if !useFamily {
		legacy = connect.NewPlatformTransport(ctx, clientStrategy, routeManager, connectUrl, auth, settings)
		return legacy, legacy, nil
	}
	// The default budget admits one carrier of each kind. The group runs three
	// transports, and at Background priority the two pins are the ones refused
	// when the budget is full, so with the default they never dial H3 and sit on
	// H1 without any signal. Size the budget for three. It is admission
	// accounting, not an allocation.
	settings.PlatformTransportBudget = familyGroupBudget()
	group = newProviderFamilyGroup(
		ctx, clientStrategySettings, clientStrategy, routeManager,
		connectUrl, familyServiceUrl(connectUrl, 4), familyServiceUrl(connectUrl, 6),
		auth, settings, connect.DefaultFamilyPlatformTransportGroupSettings(),
	)
	return group, nil, group
}

// newProviderFamilyGroup is the group constructor with explicit family urls, so
// a test can point the pins at loopback servers (familyServiceUrl refuses
// localhost and IP literals).
func newProviderFamilyGroup(
	ctx context.Context,
	clientStrategySettings *connect.ClientStrategySettings,
	clientStrategy *connect.ClientStrategy,
	routeManager *connect.RouteManager,
	connectUrl string,
	urlV4 string,
	urlV6 string,
	auth *connect.ClientAuth,
	settings *connect.PlatformTransportSettings,
	groupSettings *connect.FamilyPlatformTransportGroupSettings,
) *connect.FamilyPlatformTransportGroup {
	return connect.NewFamilyPlatformTransportGroup(
		ctx, clientStrategySettings, clientStrategy, routeManager,
		connectUrl, urlV4, urlV6, auth, connect.TransportModeAuto, settings, groupSettings,
	)
}

// familyTransportSnapshot is the last observed state of the direct identity's
// family group, read by /metrics. It is nil when the identity runs the legacy
// single transport.
type familyTransportSnapshot struct {
	Ipv4          connect.PlatformTransportState
	HasIpv4       bool
	Ipv6          connect.PlatformTransportState
	HasIpv6       bool
	Standby       connect.PlatformTransportState
	StandbyActive bool
}

var familyTransportLast atomic.Pointer[familyTransportSnapshot]

// familyTransportDecision is the last launch decision, read by /metrics.
type familyTransportDecision struct {
	Use    bool
	Reason string
}

var familyTransportDecisionLast atomic.Pointer[familyTransportDecision]

func recordFamilyTransportDecision(use bool, reason string) {
	familyTransportDecisionLast.Store(&familyTransportDecision{Use: use, Reason: reason})
	if use {
		tlog("[ipv6] direct identity platform transports: dualstack group\n")
	} else {
		tlog("[ipv6] direct identity platform transports: legacy single (reason=%s)\n", reason)
	}
}

// startFamilyStatusReporter samples the group's per-transport state every 30s
// and on each group connectivity change, publishes it for /metrics, and logs
// only when the (v4, v6, standby) tuple changes. The returned stop cancels the
// sampler, waits for it, and clears the published snapshot.
func startFamilyStatusReporter(ctx context.Context, group *connect.FamilyPlatformTransportGroup) (stop func()) {
	reporterCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go connect.HandleError(func() {
		defer wg.Done()
		var last familyTransportSnapshot
		first := true
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			notify := group.ConnectedNotify()
			status := group.Status()
			snapshot := familyTransportSnapshot{
				Ipv4: status.Ipv4, HasIpv4: status.HasIpv4,
				Ipv6: status.Ipv6, HasIpv6: status.HasIpv6,
				Standby: status.Standby, StandbyActive: status.StandbyActive,
			}
			familyTransportLast.Store(&snapshot)
			if first || snapshot != last {
				tlog("[ipv6] family transports v4=%s v6=%s standby=%s active=%t\n",
					snapshot.Ipv4, snapshot.Ipv6, snapshot.Standby, snapshot.StandbyActive)
				last = snapshot
				first = false
			}
			select {
			case <-reporterCtx.Done():
				return
			case <-ticker.C:
			case <-notify:
			}
		}
	})
	return func() {
		cancel()
		wg.Wait()
		familyTransportLast.Store(nil)
	}
}

// writeFamilyTransportMetrics emits the dual-stack decision and, when the group
// runs, each transport's state. The reason set is closed (see
// useFamilyTransports) so label cardinality stays bounded.
func writeFamilyTransportMetrics(b *strings.Builder) {
	decision := familyTransportDecisionLast.Load()
	if decision == nil {
		return
	}
	mode := "legacy"
	if decision.Use {
		mode = "group"
	}
	fmt.Fprintf(b, "# HELP urnet_dualstack_mode Direct identity platform transport mode, value always 1: group (v4 and v6 pins plus standby) or legacy (single), with the reason.\n")
	fmt.Fprintf(b, "# TYPE urnet_dualstack_mode gauge\n")
	fmt.Fprintf(b, "urnet_dualstack_mode{mode=%s,reason=%s} 1\n", prometheusLabelValue(mode), prometheusLabelValue(decision.Reason))

	snapshot := familyTransportLast.Load()
	if snapshot == nil {
		return
	}
	fmt.Fprintf(b, "# HELP urnet_dualstack_transport_state Per-transport state of the dual-stack group: 1 for the state the transport is in.\n")
	fmt.Fprintf(b, "# TYPE urnet_dualstack_transport_state gauge\n")
	write := func(transport string, has bool, state connect.PlatformTransportState) {
		if !has {
			return
		}
		fmt.Fprintf(b, "urnet_dualstack_transport_state{transport=%s,state=%s} 1\n",
			prometheusLabelValue(transport), prometheusLabelValue(state.String()))
	}
	write("ipv4", snapshot.HasIpv4, snapshot.Ipv4)
	write("ipv6", snapshot.HasIpv6, snapshot.Ipv6)
	write("standby", true, snapshot.Standby)
	active := 0
	if snapshot.StandbyActive {
		active = 1
	}
	fmt.Fprintf(b, "# HELP urnet_dualstack_standby_active Whether the family-agnostic standby is dialing because no pinned transport is connected.\n")
	fmt.Fprintf(b, "# TYPE urnet_dualstack_standby_active gauge\n")
	fmt.Fprintf(b, "urnet_dualstack_standby_active %d\n", active)
}

// familyBudgetReferenceByteCount is connect's reference memory budget, used
// when no process-wide memory target is set.
const familyBudgetReferenceByteCount = 64 << 20

// familyGroupBudget is the transport budget for one identity's family group:
// the default sizing, scaled for the group's three transports.
func familyGroupBudget() *connect.PlatformTransportBudget {
	target := connect.MemoryBudget()
	if target <= 0 {
		target = familyBudgetReferenceByteCount
	}
	return connect.NewPlatformTransportBudgetForMemoryTarget(3 * target)
}
