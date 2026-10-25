package miner

// proxy_v6_probe.go - decide whether a SOCKS5 proxy can carry IPv6, and
// remember the answer.
//
// The probe is one SOCKS5 CONNECT to a hostname on port 443 whose name has
// only AAAA records (the caller chooses the name). Handing the proxy a
// hostname, not a literal, makes the proxy pick the address family, so a
// success proves the proxy reached the target over IPv6 only when the name is
// AAAA-only. Both units here are pure: no sockets, no goroutines, no timers.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// v6Verdict is the answer to "can this proxy carry IPv6".
type v6Verdict int

const (
	v6Unknown v6Verdict = iota // no usable evidence; probe again on demand
	v6Yes
	v6No
)

func (v v6Verdict) String() string {
	switch v {
	case v6Yes:
		return "yes"
	case v6No:
		return "no"
	default:
		return "unknown"
	}
}

// MarshalText makes the verdict a stable word in JSON, both as a value and as
// a map key, so a persisted ledger survives reordering of the constants.
func (v v6Verdict) MarshalText() ([]byte, error) { return []byte(v.String()), nil }

func (v *v6Verdict) UnmarshalText(b []byte) error {
	switch string(b) {
	case "unknown":
		*v = v6Unknown
	case "yes":
		*v = v6Yes
	case "no":
		*v = v6No
	default:
		return fmt.Errorf("v6 verdict: unknown value %q", b)
	}
	return nil
}

// socksNoReply is the socksReply argument when no reply code was read, because
// the dial failed before the proxy answered the CONNECT. 0x00 is a real code
// (succeeded), so zero cannot mean "none".
const socksNoReply = -1

// RFC 1928 section 6 reply codes.
const (
	socksRepSucceeded            = 0x00
	socksRepGeneralFailure       = 0x01
	socksRepNotAllowed           = 0x02
	socksRepNetworkUnreachable   = 0x03
	socksRepHostUnreachable      = 0x04
	socksRepConnectionRefused    = 0x05
	socksRepTTLExpired           = 0x06
	socksRepCommandNotSupported  = 0x07
	socksRepAddrTypeNotSupported = 0x08
)

// classifyV6Probe turns the outcome of one SOCKS5 CONNECT to an AAAA-only
// hostname on :443 into a verdict. err is the dial error (nil on success) and
// socksReply the RFC 1928 reply code read from the proxy, or socksNoReply.
//
// The rule that drives every mapping: "no" must mean the proxy told us, or
// failed in the way an IPv4-only proxy fails, that it cannot do IPv6. Anything
// that says nothing about the proxy's address families is "unknown", never
// "no", because a wrong "no" costs a lost IPv6 route for the no TTL while
// "unknown" costs one more probe.
func classifyV6Probe(err error, socksReply int) v6Verdict {
	// A reply the proxy actually sent is the strongest evidence, so it is
	// read before anything about err. The dialer wraps the reply into err
	// ("socks connect tcp6: host unreachable"); the code is the truth.
	switch socksReply {
	case socksRepSucceeded:
		// Success needs both signals: a nil error and a success reply.
		// A success reply with an error after it (the stream broke after the
		// reply) is inconsistent, so it proves nothing.
		if err == nil {
			return v6Yes
		}
		return v6Unknown
	case socksRepHostUnreachable:
		// The proxy took the request, resolved or tried the AAAA-only name,
		// and has no route to it. This is the standard IPv4-only reply.
		return v6No
	case socksRepAddrTypeNotSupported:
		// The proxy states it cannot handle the address type, which for an
		// AAAA-only name is IPv6 itself. Definitive.
		return v6No
	case socksRepNotAllowed:
		// Ruleset forbids this connection. For this probe the only thing
		// different from the proxy's normal traffic is the IPv6 target, and
		// a proxy that forbids it will forbid the real dials too. Treated as
		// "no": we cannot use a route the proxy refuses to give.
		return v6No
	case socksRepNetworkUnreachable:
		// Same meaning as 0x04 for our purposes: no IPv6 network path.
		return v6No
	case socksRepGeneralFailure, socksRepConnectionRefused, socksRepTTLExpired, socksRepCommandNotSupported:
		// 0x01 is a catch-all (overload, internal error) and could hit any
		// family. 0x05 means the target host refused, which suggests the
		// proxy did reach it over IPv6, but a refusal from the far end is not
		// a success, so we do not promote it to "yes". 0x06 is a transit
		// problem, not a capability answer. 0x07 says the proxy lacks CONNECT
		// and says nothing about IPv6. None of these is evidence either way.
		return v6Unknown
	}
	// No usable reply: any other code is undefined by RFC 1928, and
	// socksNoReply means the failure happened before the proxy spoke.

	if err == nil {
		// No error and no reply: we were not told that anything happened.
		return v6Unknown
	}

	// Our own cancellation or deadline says nothing about the proxy. This is
	// checked before EOF because cancelling a dial closes the conn, which
	// can surface as an EOF that the proxy did not cause.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return v6Unknown
	}

	// The proxy closed the connection after our request without a reply.
	// IPv4-only SOCKS5 servers that cannot make an IPv6 connection often just
	// drop the stream, and this is the dominant failure on the LA7 pool:
	// `socks connect tcp6 ...: unexpected EOF`. It is also what an overloaded
	// or restarting proxy does, which the single probe cannot tell apart;
	// the short no TTL bounds the damage of that misread, and the caller
	// should compare against an IPv4 probe to the same proxy if it wants to
	// exclude a dead proxy.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return v6No
	}

	// Connection refused by the proxy itself: nothing is listening, so the
	// probe never reached the SOCKS layer. Says nothing about IPv6.
	if errors.Is(err, syscall.ECONNREFUSED) {
		return v6Unknown
	}

	// I/O timeouts: a slow proxy and a black-holed IPv6 path look identical
	// from here. Re-probing on demand is cheap, a false "no" is not.
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return v6Unknown
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return v6Unknown
	}

	// Anything unrecognized (DNS failure for the proxy's own name, TLS to an
	// https proxy, auth failure) is not evidence about IPv6.
	return v6Unknown
}

// Trust windows and the in-service failure rule.
const (
	// v6YesTTL: an IPv6 route that worked is unlikely to disappear within a
	// few hours, and re-probing 500 proxies more often costs dials.
	// UNVERIFIED as a measured value: it is the operator's chosen policy.
	v6YesTTL = 6 * time.Hour
	// v6NoTTL: short, so a proxy that gains IPv6 (or whose "no" was a
	// transient misread) is picked up quickly without hammering it.
	// UNVERIFIED as a measured value: it is the operator's chosen policy.
	v6NoTTL = 15 * time.Minute
	// v6FlipAfter consecutive in-service failures turn a live "yes" into "no".
	v6FlipAfter = 3
)

// v6Entry is one proxy's record.
type v6Entry struct {
	Verdict   v6Verdict `json:"verdict"`
	CheckedAt time.Time `json:"checked_at"`
	// Failures counts consecutive in-service failures while the verdict is
	// yes. It is persisted so a restart does not hand a failing proxy a
	// fresh budget.
	Failures int `json:"failures,omitempty"`
}

// ttl returns the trust window of the entry's verdict; zero means "never
// trusted" (unknown).
func (e v6Entry) ttl() time.Duration {
	switch e.Verdict {
	case v6Yes:
		return v6YesTTL
	case v6No:
		return v6NoTTL
	}
	return 0
}

// v6Ledger records one v6Verdict per proxy address. The zero value is not
// usable; build with newV6Ledger. Safe for concurrent use.
type v6Ledger struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]v6Entry
}

// newV6Ledger takes the clock so tests control time; nil means time.Now.
func newV6Ledger(now func() time.Time) *v6Ledger {
	if now == nil {
		now = time.Now
	}
	return &v6Ledger{now: now, entries: map[string]v6Entry{}}
}

// live reports whether e is still inside its trust window at t. The window is
// half open: trusted for ages in [0, ttl). A negative age (clock stepped back,
// or a persisted entry from a future clock) is not trusted, so a bad clock
// can only cause an extra probe, never a stale verdict that lasts.
func (e v6Entry) live(t time.Time) bool {
	ttl := e.ttl()
	if ttl == 0 {
		return false
	}
	age := t.Sub(e.CheckedAt)
	return age >= 0 && age < ttl
}

// Verdict is the trusted answer for addr: yes or no inside the window,
// unknown for a missing, unknown or expired entry. A caller probes on
// unknown.
func (l *v6Ledger) Verdict(addr string) v6Verdict {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[addr]
	if !ok || !e.live(l.now()) {
		return v6Unknown
	}
	return e.Verdict
}

// Record stores a probe result. An unknown result is no new information, so
// it never replaces a live yes or no; it only stamps a missing or expired
// entry. A yes or no always replaces and restarts the window and the failure
// count.
func (l *v6Ledger) Record(addr string, v v6Verdict) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	if v == v6Unknown {
		if e, ok := l.entries[addr]; ok && e.live(t) {
			return
		}
	}
	l.entries[addr] = v6Entry{Verdict: v, CheckedAt: t}
}

// ServiceFailure reports a failed IPv6 use of a proxy that is in service. It
// counts only against a live yes; after v6FlipAfter in a row the verdict
// becomes no and the no window starts now. It returns true on the flip.
func (l *v6Ledger) ServiceFailure(addr string) (flipped bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	e, ok := l.entries[addr]
	if !ok || e.Verdict != v6Yes || !e.live(t) {
		return false
	}
	e.Failures++
	if e.Failures >= v6FlipAfter {
		l.entries[addr] = v6Entry{Verdict: v6No, CheckedAt: t}
		return true
	}
	l.entries[addr] = e
	return false
}

// ServiceSuccess clears the consecutive-failure count of a yes. It does not
// extend CheckedAt: trust comes from a probe, so a busy proxy still gets
// re-probed every v6YesTTL.
func (l *v6Ledger) ServiceSuccess(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[addr]; ok && e.Failures != 0 {
		e.Failures = 0
		l.entries[addr] = e
	}
}

// v6LedgerFile is the persisted shape. The version lets a later format change
// be refused instead of misread.
type v6LedgerFile struct {
	Version int                `json:"version"`
	Entries map[string]v6Entry `json:"entries"`
}

const v6LedgerVersion = 1

// MarshalJSON snapshots the ledger.
func (l *v6Ledger) MarshalJSON() ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return json.Marshal(v6LedgerFile{Version: v6LedgerVersion, Entries: l.entries})
}

// UnmarshalJSON replaces the entries and keeps the clock. Expired entries are
// kept as read; Verdict already treats them as unknown.
func (l *v6Ledger) UnmarshalJSON(b []byte) error {
	var f v6LedgerFile
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	if f.Version != v6LedgerVersion {
		return fmt.Errorf("v6 ledger: unsupported version %d", f.Version)
	}
	if f.Entries == nil {
		f.Entries = map[string]v6Entry{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = f.Entries
	return nil
}
