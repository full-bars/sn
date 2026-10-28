package miner

// host_capability.go records what this host can actually reach, so the
// activation budget charges a pass for the posts it will really spend.
//
// A box without usable IPv6 posts once per pass, not twice. Charging it two
// posts anyway halves the passes it is allowed in the operator's six-per-hour
// window for no reason, so the capability is measured and stored rather than
// assumed.

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	// providerStoreKeyHostCapability is the store record this file owns.
	providerStoreKeyHostCapability = "host_capability"

	// The target is a resolver, so the probe asks it a real question and waits
	// for the answer. A send the kernel accepts proves only that a route
	// exists: a blackholed v6 path accepts the write and returns nothing, and
	// that is indistinguishable from a working path until something comes
	// back.
	ipv6ProbeTarget    = "[2001:4860:4860::8888]:53"
	ipv6ProbeQueryName = "example.com"
	ipv6ProbeTimeout   = 1500 * time.Millisecond

	// ipv6ProbeReplyMax is one udp dns message: the classic 512-byte limit,
	// which any answer to this query fits in.
	ipv6ProbeReplyMax = 512

	// ipv6ProbeAttempts is how many times one measurement asks before it
	// calls v6 unusable. A probe is a single udp exchange, so one lost packet
	// would otherwise downgrade a host for a whole recheck interval; a second
	// question costs one round trip and keeps a working path measured working.
	ipv6ProbeAttempts = 2

	// Cadence. A host that has had IPv6 recently is probably flapping or
	// waiting for a tunnel, and restoring it doubles what a pass earns, so it
	// is re-checked often. A host that has never had it is re-checked rarely;
	// an address change triggers an immediate check anyway.
	capabilityRecheckHadIPv6Recently = 15 * time.Minute
	capabilityRecheckHadIPv6LongAgo  = time.Hour
	capabilityRecheckNeverHadIPv6    = 6 * time.Hour
	capabilityRecentWindow           = 24 * time.Hour

	// interfaceFingerprintInterval is how often the address list is compared
	// without sending anything. A change means a network change, which is the
	// moment to spend a probe.
	interfaceFingerprintInterval = 3 * time.Minute
)

// hostCapability is the stored measurement.
type hostCapability struct {
	// IPv6Usable is the last measurement.
	IPv6Usable bool `json:"ipv6_usable"`
	// EverUsable records that this host has had usable IPv6 at some point,
	// which separates "flapping" from "never had it" for the recheck cadence.
	EverUsable bool `json:"ever_usable,omitempty"`
	// CheckedAtUnix is when the measurement was taken.
	CheckedAtUnix int64 `json:"checked_at_unix,omitempty"`
	// LastIPv6UsableAtUnix is the last time it measured usable.
	LastIPv6UsableAtUnix int64 `json:"last_ipv6_usable_at_unix,omitempty"`
	// LastError keeps the reason a probe failed, for the status line.
	LastError string `json:"last_error,omitempty"`
}

// readHostCapability loads the stored measurement. A host that has never been
// measured and a record that could not be read both give the zero value, so a
// caller that only reports or schedules with it can ignore the error; the one
// that writes the next measurement must not, or it replaces the history it
// failed to read with a record built from nothing.
func readHostCapability() (hostCapability, error) {
	var cap hostCapability
	store := providerStoreInstance()
	if store == nil {
		return cap, errProviderStoreClosed
	}
	if _, err := store.load(providerStoreBucketState, providerStoreKeyHostCapability, &cap); err != nil {
		return hostCapability{}, err
	}
	return cap, nil
}

func writeHostCapability(cap hostCapability) {
	if store := providerStoreInstance(); store != nil {
		_ = store.save(providerStoreBucketState, providerStoreKeyHostCapability, cap)
	}
}

// ipv6ProbeConn is the part of a connected udp socket the probe needs. A test
// supplies net.Pipe rather than a real socket.
type ipv6ProbeConn interface {
	SetWriteDeadline(time.Time) error
	SetReadDeadline(time.Time) error
	Write([]byte) (int, error)
	Read([]byte) (int, error)
	Close() error
}

// ipv6GlobalAddressPresent is the address precheck. It is a variable so a test
// can show the short circuit without depending on this host's own addresses.
var ipv6GlobalAddressPresent = hasGlobalIPv6Address

// probeIPv6Usable asks the resolver a dns question over IPv6 and reports
// whether an answer came back. A host with no global v6 address is rejected
// without a syscall, and an answer that does not answer this query is not
// usable: the operator spends activation posts on what this says.
func probeIPv6Usable() (bool, string) {
	if !ipv6GlobalAddressPresent() {
		return false, "no global ipv6 address"
	}
	return probeIPv6With(func() (bool, string) {
		return probeIPv6ReplyOver(dialIPv6Probe, ipv6ProbeTimeout)
	})
}

// probeIPv6With runs one measurement up to ipv6ProbeAttempts times and reports
// the last reason it did not answer. One lost udp packet is not evidence that a
// host has no v6: the alternation lives here so a test can show the retry
// without waiting out a real deadline twice.
func probeIPv6With(measure func() (bool, string)) (bool, string) {
	reason := ""
	for attempt := 0; attempt < ipv6ProbeAttempts; attempt++ {
		usable, attemptReason := measure()
		if usable {
			return true, ""
		}
		reason = attemptReason
	}
	return false, reason
}

// dialIPv6Probe opens the one socket a measurement uses.
func dialIPv6Probe() (ipv6ProbeConn, error) {
	return net.DialTimeout("udp6", ipv6ProbeTarget, ipv6ProbeTimeout)
}

// probeIPv6ReplyOver dials, asks, and leaves closing to probeIPv6Reply, which
// owns the socket for every path that can fail after the dial.
func probeIPv6ReplyOver(dial func() (ipv6ProbeConn, error), timeout time.Duration) (bool, string) {
	conn, err := dial()
	if err != nil {
		return false, err.Error()
	}
	return probeIPv6Reply(conn, timeout)
}

// probeIPv6Reply runs one query over conn and reports whether a reply answered
// it, with the reason it did not in the second value. It takes ownership of
// conn and closes it: a measurement that cannot build its query would
// otherwise leak the socket it just opened.
func probeIPv6Reply(conn ipv6ProbeConn, timeout time.Duration) (bool, string) {
	defer conn.Close()
	id, query, err := newIPv6ProbeQueryFn()
	if err != nil {
		return false, err.Error()
	}
	reply, err := ipv6ProbeExchange(conn, query, timeout)
	if err != nil {
		return false, err.Error()
	}
	if err := checkIPv6ProbeReply(reply, id); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// ipv6ProbeExchange writes the query and waits for one reply. The wait is what
// the probe measures: a path that accepts the send and returns nothing is not
// usable, and only the read can tell that apart from a path that works. The
// caller owns the socket and closes it.
func ipv6ProbeExchange(conn ipv6ProbeConn, query []byte, timeout time.Duration) ([]byte, error) {
	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	reply := make([]byte, ipv6ProbeReplyMax)
	n, err := conn.Read(reply)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, fmt.Errorf("query sent, nothing came back within %s: %w", timeout, err)
		}
		return nil, err
	}
	return reply[:n], nil
}

// checkIPv6ProbeReply reports why a reply does not answer the query. A response
// is the least a reply has to be: an echo of our own query carries this id and
// proves the send direction only, so the QR bit is required before the id is
// even worth comparing.
//
// An NXDOMAIN (rcode 3) counts as reachability evidence. The measurement asks
// whether the resolver answered at all, and "no such name" is an answer that
// crossed the v6 path and came back; treating it as a probe failure would
// downgrade a working host. Every other rcode is likewise an answer.
func checkIPv6ProbeReply(reply []byte, id uint16) error {
	if len(reply) < 12 {
		return fmt.Errorf("reply is %d bytes, too short for a dns header", len(reply))
	}
	if reply[2]&0x80 == 0 {
		return fmt.Errorf("reply does not have the response bit set")
	}
	if got := binary.BigEndian.Uint16(reply[0:2]); got != id {
		return fmt.Errorf("reply id %d does not answer query id %d", got, id)
	}
	return nil
}

// newIPv6ProbeQueryFn builds the query. A variable so a test can show the
// socket is closed even on the path where the query cannot be built.
var newIPv6ProbeQueryFn = newIPv6ProbeQuery

// newIPv6ProbeQuery builds a minimal A query with a random id: a 12-byte
// header with recursion desired and one question, so any conforming resolver
// answers it and the answer is evidence about the path.
func newIPv6ProbeQuery() (uint16, []byte, error) {
	var raw [2]byte
	if _, err := cryptorand.Read(raw[:]); err != nil {
		return 0, nil, err
	}
	id := binary.BigEndian.Uint16(raw[:])
	return id, ipv6ProbeQuery(id), nil
}

// ipv6ProbeQuery is the query wire form for one id.
func ipv6ProbeQuery(id uint16) []byte {
	name := make([]byte, 0, len(ipv6ProbeQueryName)+2)
	for _, label := range strings.Split(ipv6ProbeQueryName, ".") {
		name = append(name, byte(len(label)))
		name = append(name, label...)
	}
	name = append(name, 0)

	query := make([]byte, 12, 12+len(name)+4)
	binary.BigEndian.PutUint16(query[0:2], id)
	// The flags word: recursion desired, and this is a query, not a response.
	binary.BigEndian.PutUint16(query[2:4], 0x0100)
	binary.BigEndian.PutUint16(query[4:6], 1)
	query = append(query, name...)
	query = binary.BigEndian.AppendUint16(query, 1)
	query = binary.BigEndian.AppendUint16(query, 1)
	return query
}

// hasGlobalIPv6Address reports whether any interface carries a routable v6
// address. Loopback and link-local do not count: neither can reach the
// platform.
func hasGlobalIPv6Address() bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.To4() != nil {
			continue
		}
		ip := ipnet.IP
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			continue
		}
		return true
	}
	return false
}

// capabilityRecheckInterval is how long to wait before measuring again, given
// what the record says about this host's history.
func capabilityRecheckInterval(cap hostCapability, now time.Time) time.Duration {
	if cap.IPv6Usable {
		return capabilityRecheckHadIPv6Recently
	}
	if !cap.EverUsable {
		return capabilityRecheckNeverHadIPv6
	}
	if cap.LastIPv6UsableAtUnix > 0 && now.Sub(time.Unix(cap.LastIPv6UsableAtUnix, 0)) <= capabilityRecentWindow {
		return capabilityRecheckHadIPv6Recently
	}
	return capabilityRecheckHadIPv6LongAgo
}

// measureHostCapability probes once and stores the result. A record that could
// not be read is not an empty record: writing the measurement back over it
// would drop the history that decides the recheck cadence and the post count,
// so the measurement is reported to this process only.
func measureHostCapability(now time.Time) hostCapability {
	usable, probeErr := probeIPv6Usable()
	cap, err := readHostCapability()
	if err != nil {
		tlog("⚠️ [capability] measured ipv6 usable=%t but the record could not be read (%v): not stored\n", usable, err)
		measured := hostCapability{IPv6Usable: usable, CheckedAtUnix: now.Unix(), LastError: probeErr}
		if usable {
			measured.EverUsable = true
			measured.LastIPv6UsableAtUnix = now.Unix()
		}
		return measured
	}
	cap.IPv6Usable = usable
	cap.CheckedAtUnix = now.Unix()
	cap.LastError = probeErr
	if usable {
		cap.EverUsable = true
		cap.LastIPv6UsableAtUnix = now.Unix()
	}
	writeHostCapability(cap)
	return cap
}

// watchHostCapability measures at startup, then re-measures on the cadence the
// record implies, and immediately when the interface address list changes.
// It never blocks startup and stops when the context ends.
func watchHostCapability(ctx context.Context) {
	now := time.Now()
	// Startup always measures: the previous value may be days old, and the
	// first pass should charge the right number of posts.
	cap := measureHostCapability(now)
	fingerprint := interfaceFingerprint()

	next := now.Add(capabilityRecheckInterval(cap, now))
	ticker := time.NewTicker(interfaceFingerprintInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case at := <-ticker.C:
			changed := false
			if f := interfaceFingerprint(); f != fingerprint {
				fingerprint = f
				changed = true
			}
			if !changed && at.Before(next) {
				continue
			}
			cap = measureHostCapability(at)
			next = at.Add(capabilityRecheckInterval(cap, at))
		}
	}
}

// interfaceFingerprint is a cheap digest of the interface address list. It
// costs no packets and changes when the host's addressing changes.
func interfaceFingerprint() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	b := make([]byte, 0, len(addrs)*16)
	for _, a := range addrs {
		b = append(b, a.String()...)
		b = append(b, '\n')
	}
	return string(b)
}

// hostActivationPosts is how many posts one activation pass spends here: one
// per address family the host can actually reach. An unmeasured host is
// charged two, the conservative side, so a missing record never overspends.
func hostActivationPosts() int {
	cap, _ := readHostCapability()
	if cap.IPv6Usable {
		return 2
	}
	if cap.CheckedAtUnix == 0 {
		return 2
	}
	return 1
}
