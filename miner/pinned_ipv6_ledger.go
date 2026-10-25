package miner

// What a proxy has proven about IPv6, remembered across restarts.
//
// The LA7 diagnosis (#5070, #5081, DESIGN-sn-miner-leaks.md P2) found the miner
// starting a pinned IPv6 transport for every identity and hammering it through
// proxies that cannot carry IPv6 at all: 43,620 failed tcp6 dials in 6.5
// minutes, ~209 successes, one core pegged. Each restart re-learned the same
// fact from scratch, because nothing survived the process.
//
// This is the memory. It is deliberately small and verdict-shaped rather than a
// failure counter: the question the dial path needs answered is "should this
// identity's v6 transport exist at all", and the answer only changes on
// evidence. Positive evidence is preferred - a proxy that once carried tcp6 is
// allowed to try again for six hours - because parking IPv6 on a proxy that
// supports it costs the operator reach they were told not to give up.
//
// The classes exist so the operator can tell "this proxy said no" from "I could
// not ask". Only a definitive refusal parks v6. A timeout, a dead proxy or a
// proxy we could not reach says nothing about IPv6, and parking on that would
// quietly disable IPv6 for every proxy that happened to be slow at start.

import (
	"errors"
	"net"
	"strings"
	"syscall"
	"time"
)

// pinnedIpv6Verdict is what one proxy has proven about IPv6.
//
// "unknown" is not a synonym for "no": it means no usable evidence yet, and the
// policy keeps v6 held while it probes rather than parking it forever.
type pinnedIpv6Verdict string

const (
	pinnedIpv6Unknown pinnedIpv6Verdict = "unknown"
	pinnedIpv6Yes     pinnedIpv6Verdict = "yes"
	pinnedIpv6No      pinnedIpv6Verdict = "no"
)

// Freshness windows. A yes lasts six hours because a proxy that carries v6
// today usually still will, and re-proving it is cheap only if something else
// is not already using the box. A no lasts fifteen minutes because the failure
// modes that produce it (a proxy restarting, a node losing its v6 route) do
// clear, and the operator asked for IPv6 to be used wherever it works.
const (
	pinnedIpv6YesFreshness = 6 * time.Hour
	pinnedIpv6NoFreshness  = 15 * time.Minute
)

// pinnedIpv6FailuresToPark is how many isolated v6 failures in a row, with the
// proxy otherwise working, flip a proxy from "unknown"/"yes" to "no". Three
// because a single failed dial is noise on a busy box and the existing 5 s
// doubling backoff already absorbs two; three in a row while IPv4 through the
// same proxy answers is a proxy that does not carry v6.
const pinnedIpv6FailuresToPark = 3

// Probe classes, recorded beside the verdict so the operator can see WHY a
// proxy is parked without reading the dial path.
const (
	pinnedIpv6ClassConnected      = "connected"
	pinnedIpv6ClassAddrType       = "address_type_not_supported"
	pinnedIpv6ClassHostUnreach    = "host_unreachable"
	pinnedIpv6ClassRuleset        = "not_allowed_by_ruleset"
	pinnedIpv6ClassRefused        = "connection_refused"
	pinnedIpv6ClassImmediateEOF   = "immediate_eof"
	pinnedIpv6ClassTimeout        = "timeout"
	pinnedIpv6ClassProxyUnreach   = "proxy_unreachable"
	pinnedIpv6ClassUnknown        = "unknown"
	pinnedIpv6ClassIsolatedFailed = "isolated_failures"
)

// pinnedIpv6State is the persisted half of the ledger.
type pinnedIpv6State struct {
	Verdict           pinnedIpv6Verdict
	CheckedAt         time.Time
	Class             string
	ConsecutiveFailed int
}

// pinnedIpv6Decision is what the slot start should do with v6 right now.
type pinnedIpv6Decision struct {
	// EnablePinnedV6 starts the pinned IPv6 transport immediately, without
	// waiting for a probe. Only a fresh "yes" earns this.
	EnablePinnedV6 bool
	// Probe asks for a background probe. The slot never waits for it: IPv4 and
	// the family-agnostic standby start at once either way.
	Probe bool
	// Reason is one operator-facing phrase for the start line.
	Reason string
}

// pinnedIpv6DecideAtStart is the policy at slot start.
//
// A fresh verdict is used as-is: yes enables v6 with no probe, no holds it
// until the verdict goes stale. Unknown or stale probes in the background and
// holds v6 until the answer arrives, which is the whole point - the restart
// that used to start 500 tcp6 storms now starts none.
func pinnedIpv6DecideAtStart(state pinnedIpv6State, now time.Time) pinnedIpv6Decision {
	switch state.Verdict {
	case pinnedIpv6Yes:
		if pinnedIpv6Fresh(state, now) {
			return pinnedIpv6Decision{EnablePinnedV6: true, Reason: "ipv6 proven " + pinnedIpv6Age(state.CheckedAt, now)}
		}
		return pinnedIpv6Decision{Probe: true, Reason: "ipv6 verdict stale (" + pinnedIpv6Age(state.CheckedAt, now) + ")"}
	case pinnedIpv6No:
		if pinnedIpv6Fresh(state, now) {
			return pinnedIpv6Decision{Reason: "ipv6 parked: " + pinnedIpv6ClassLabel(state.Class) + " " + pinnedIpv6Age(state.CheckedAt, now)}
		}
		return pinnedIpv6Decision{Probe: true, Reason: "ipv6 parked, re-probing (verdict " + pinnedIpv6Age(state.CheckedAt, now) + ")"}
	default:
		// No verdict at all, or a verdict written by an older build.
		return pinnedIpv6Decision{Probe: true, Reason: "ipv6 unknown, probing"}
	}
}

// pinnedIpv6Fresh reports whether the verdict is inside its window.
func pinnedIpv6Fresh(state pinnedIpv6State, now time.Time) bool {
	window := pinnedIpv6NoFreshness
	if state.Verdict == pinnedIpv6Yes {
		window = pinnedIpv6YesFreshness
	}
	// A zero CheckedAt is a state file written before this field existed. It is
	// not fresh: the run must re-prove rather than trust a timestamp it does
	// not have.
	if state.CheckedAt.IsZero() {
		return false
	}
	return now.Sub(state.CheckedAt) < window
}

// notePinnedIpv6Success records a successful tcp6 dial through this proxy. One
// success outranks any number of earlier failures: the proxy just proved it
// carries IPv6, which is the only evidence the operator asked for.
func notePinnedIpv6Success(state pinnedIpv6State, now time.Time) pinnedIpv6State {
	return pinnedIpv6State{
		Verdict:   pinnedIpv6Yes,
		CheckedAt: now,
		Class:     pinnedIpv6ClassConnected,
	}
}

// notePinnedIpv6Failure records one v6 dial failure that is worth counting, and
// reports whether it parked the proxy.
//
// "Isolated" is the caller's promise: this failure happened while the platform
// answered other dials, so it is evidence about this proxy's IPv6 and not about
// the network being down. The agnostic transport's long tier uses the same
// rule, for the same reason.
func notePinnedIpv6Failure(state pinnedIpv6State, now time.Time) (pinnedIpv6State, bool) {
	state.ConsecutiveFailed++
	if state.ConsecutiveFailed < pinnedIpv6FailuresToPark {
		return state, false
	}
	return pinnedIpv6State{
		Verdict:   pinnedIpv6No,
		CheckedAt: now,
		Class:     pinnedIpv6ClassIsolatedFailed,
	}, true
}

// classifyPinnedIpv6Probe turns a probe error into a verdict and a class.
//
// Only a definitive refusal counts as "no". Everything ambiguous - a timeout, a
// proxy that would not answer, a TLS handshake that died - is "unknown", which
// parks nothing and re-probes soon. Parking on ambiguity is how a slow proxy at
// boot would silently cost the operator IPv6.
func classifyPinnedIpv6Probe(err error, sawEOF bool) (pinnedIpv6Verdict, string) {
	if err == nil {
		return pinnedIpv6Yes, pinnedIpv6ClassConnected
	}
	text := strings.ToLower(err.Error())

	// SOCKS5 reply codes arrive as text from the dialer; match the ones that are
	// definitive about the address family.
	switch {
	case strings.Contains(text, "address type not supported"):
		return pinnedIpv6No, pinnedIpv6ClassAddrType
	case strings.Contains(text, "not allowed by ruleset"):
		return pinnedIpv6No, pinnedIpv6ClassRuleset
	case strings.Contains(text, "host unreachable"):
		return pinnedIpv6No, pinnedIpv6ClassHostUnreach
	case strings.Contains(text, "connection refused"):
		// The proxy answered and refused the target: it can carry the request,
		// the far side is not listening. That is not an IPv6 verdict about this
		// proxy, so it stays unknown rather than parking the family.
		return pinnedIpv6Unknown, pinnedIpv6ClassRefused
	case sawEOF:
		// The proxy accepted the request and closed without a reply. In the LA7
		// dumps this is the IPv4-only proxy's way of rejecting a tcp6 target.
		return pinnedIpv6No, pinnedIpv6ClassImmediateEOF
	}

	// Errors that are about reaching the proxy itself, not about the family.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return pinnedIpv6Unknown, pinnedIpv6ClassTimeout
	}
	if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) ||
		strings.Contains(text, "no route to host") {
		return pinnedIpv6Unknown, pinnedIpv6ClassProxyUnreach
	}
	if errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(text, "connection refused") {
		return pinnedIpv6Unknown, pinnedIpv6ClassProxyUnreach
	}
	return pinnedIpv6Unknown, pinnedIpv6ClassUnknown
}

// pinnedIpv6Age renders how old the verdict is for the operator's start line.
func pinnedIpv6Age(checkedAt time.Time, now time.Time) string {
	if checkedAt.IsZero() {
		return "never checked"
	}
	age := now.Sub(checkedAt)
	if age < 0 {
		// A clock step backwards must not print a negative age or a future
		// verdict; treat it as due for re-proof.
		return "checked in the future"
	}
	if age < time.Minute {
		return "just now"
	}
	if age < time.Hour {
		return time.Duration(age.Minutes()).Round(time.Minute).String() + " ago"
	}
	return time.Duration(age.Hours()).Round(time.Hour).String() + " ago"
}

// pinnedIpv6ClassLabel turns a recorded class into the words the operator sees.
func pinnedIpv6ClassLabel(class string) string {
	switch class {
	case pinnedIpv6ClassConnected:
		return "connected"
	case pinnedIpv6ClassAddrType:
		return "proxy cannot address IPv6 (address type not supported)"
	case pinnedIpv6ClassRuleset:
		return "proxy policy blocks IPv6 (not allowed by ruleset)"
	case pinnedIpv6ClassHostUnreach:
		return "no IPv6 route through the proxy (host unreachable)"
	case pinnedIpv6ClassImmediateEOF:
		return "proxy closed the IPv6 request without a reply"
	case pinnedIpv6ClassIsolatedFailed:
		return "3 IPv6 dials failed while IPv4 worked"
	case pinnedIpv6ClassTimeout:
		return "probe timed out"
	case pinnedIpv6ClassProxyUnreach:
		return "proxy unreachable"
	case pinnedIpv6ClassRefused:
		return "target refused"
	case "":
		return "no class recorded"
	default:
		return class
	}
}

// pinnedIpv6State is carried in the proxy entry, so the ledger survives a
// restart exactly as the health grade does. An entry written by a build without
// these fields reads back as the empty state, which the policy treats as
// unknown and probes - never as a verdict.
func (entry ProxyEntry) pinnedIpv6State() pinnedIpv6State {
	return pinnedIpv6State{
		Verdict:           pinnedIpv6Verdict(entry.Ipv6Verdict),
		CheckedAt:         entry.Ipv6CheckedAt,
		Class:             entry.Ipv6Class,
		ConsecutiveFailed: entry.Ipv6Failures,
	}
}

func (entry *ProxyEntry) setPinnedIpv6State(state pinnedIpv6State) {
	entry.Ipv6Verdict = string(state.Verdict)
	if state.Verdict == pinnedIpv6Unknown {
		entry.Ipv6Verdict = ""
	}
	entry.Ipv6CheckedAt = state.CheckedAt
	entry.Ipv6Class = state.Class
	entry.Ipv6Failures = state.ConsecutiveFailed
}
