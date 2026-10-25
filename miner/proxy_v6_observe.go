package miner

// The IPv6 evidence collector that needs no probe and no extra goroutine.
//
// The merged ledger (proxy_v6_probe.go) can hold a per-proxy verdict, but
// something has to produce the evidence. Polling the transport group's status
// is the wrong source: FamilyPlatformTransportGroupStatus.HasIpv6 means "a v6
// transport was configured", fixed at construction, not "the v6 pin connected",
// and sn has no status tick to poll anyway.
//
// The dial path already answers the question. When the miner injects a
// DialContextSettings, connect hands the v6 pin's dial straight to it with the
// network narrowed to tcp6 and the target left as the connect-v6 hostname. So
// every v6-pin attempt passes through this wrapper with exactly the inputs
// classifyV6Probe wants: the dial error and the SOCKS reply code inside it.
//
// connect-v6.bringyour.com has no A record (verified: A is empty, AAAA is the
// v6 address), so a completed SOCKS5 connect there really is v6 egress and not
// a v4 proxy answering on a v6-named host.
//
// Two rules keep a flaky proxy from losing dual-stack:
//   - a "no" needs v6FlipAfter classified failures, not one;
//   - it also needs a v4 or agnostic platform dial through the same slot to
//     have succeeded recently, so an outage of the proxy or of the platform
//     reads as unknown instead of as an IPv6 verdict.

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// errV6Held refuses a v6-pin dial locally once the proxy is known not to carry
// IPv6. The pin still backs off on its own schedule, but at zero network cost,
// and a kick only buys another cheap local refusal.
var errV6Held = errors.New("ipv6 not carried by this proxy (held locally)")

// v6CorroborationWindow is how recently a v4 or agnostic platform dial must
// have succeeded for a v6 failure to count. A few minutes covers a retry cycle
// and still reads a proxy outage as unknown.
const v6CorroborationWindow = 5 * time.Minute

// v6DialObserver wraps one slot's dial.
//
// It is installed inside the bandwidth wrap so it sees the dial errors before
// anything rewrites them.
type v6DialObserver struct {
	inner connect.DialContextFunction
	// host is the connect-v6 hostname this slot's pin dials.
	host string
	// key is the ledger key for this slot's proxy.
	key string
	// corroboratingHosts are the platform hosts whose successful dials prove
	// the proxy is alive, so a v6 failure is about IPv6 and not about a dead
	// proxy. The v4 and agnostic platform hosts belong here; exit-NAT client
	// traffic does not.
	corroboratingHosts map[string]bool
	ledger             *v6Ledger
	// onChange is called when a verdict is written, so the caller can persist.
	onChange func()
	now      func() time.Time

	mu             sync.Mutex
	failures       int
	corroboratedAt time.Time
}

// hostOf returns the host part of host:port, or "" when addr is not in that
// shape.
func hostOf(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return host
}

// urlHost returns the host of a platform url, which may carry a path or a
// scheme. Empty in, empty out.
func urlHost(rawUrl string) string {
	if rawUrl == "" {
		return ""
	}
	if u, err := url.Parse(rawUrl); err == nil && u.Host != "" {
		return u.Hostname()
	}
	if host := hostOf(rawUrl); host != "" {
		return host
	}
	return rawUrl
}

// DialContext passes everything through and records IPv6 evidence for the
// connect-v6 hostname only.
func (self *v6DialObserver) DialContext(ctx context.Context, network string, addr string) (net.Conn, error) {
	host := hostOf(addr)
	if host == "" {
		return self.inner(ctx, network, addr)
	}

	if network != "tcp6" || host != self.host {
		conn, err := self.inner(ctx, network, addr)
		if err == nil && self.corroboratingHosts[host] {
			self.mu.Lock()
			self.corroboratedAt = self.now()
			self.mu.Unlock()
		}
		return conn, err
	}

	// A proxy already known not to carry v6: refuse without touching the
	// network. This is what stops the retry storm during the run.
	if self.ledger.Verdict(self.key) == v6No {
		return nil, errV6Held
	}

	conn, err := self.inner(ctx, network, addr)
	if err == nil {
		self.ledger.Record(self.key, v6Yes)
		self.ledger.ServiceSuccess(self.key)
		self.mu.Lock()
		self.failures = 0
		self.mu.Unlock()
		self.changed()
		return conn, nil
	}

	// Our own cancel is not evidence about the proxy's IPv6. classV6Probe
	// already treats a bare cancel as unknown, but a cancel can surface as an
	// EOF after the SOCKS stream closes, and classifyV6Probe reads that as no.
	if ctx.Err() != nil {
		return conn, err
	}

	verdict := classifyV6Probe(err, socksReplyFromError(err))
	switch verdict {
	case v6Yes:
		// A reply of 0x00 that still failed on the stream means no usable
		// evidence; classifyV6Probe returns unknown for that and we are here
		// only if it did not.
		self.ledger.Record(self.key, v6Yes)
		self.changed()
	case v6No:
		// A live yes downgrades through the ledger's own counter, so a proxy
		// that worked minutes ago flips on the same three-strike rule.
		if self.ledger.Verdict(self.key) == v6Yes {
			if self.ledger.ServiceFailure(self.key) {
				self.changed()
			}
			return conn, err
		}
		if !self.corroborated() {
			// The proxy or the platform is down: this says nothing about IPv6.
			return conn, err
		}
		self.mu.Lock()
		self.failures++
		flip := self.failures >= v6FlipAfter
		if flip {
			self.failures = 0
		}
		self.mu.Unlock()
		if flip {
			self.ledger.Record(self.key, v6No)
			self.changed()
		}
	}
	return conn, err
}

// corroborated reports whether a v4 or agnostic platform dial through this slot
// succeeded recently.
func (self *v6DialObserver) corroborated() bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	return !self.corroboratedAt.IsZero() &&
		self.now().Sub(self.corroboratedAt) < v6CorroborationWindow
}

func (self *v6DialObserver) changed() {
	if self.onChange != nil {
		self.onChange()
	}
}

// v6LedgerKey scopes a verdict to the proxy that produced it. Rotating gateways
// keep one host:port while the exit changes, and many encode the session in the
// SOCKS username, so the address alone would merge unrelated exits. A changed
// address is simply a new key, which reads unknown and gets the group once.
func v6LedgerKey(settings *connect.ProxySettings) string {
	if settings == nil {
		return ""
	}
	if settings.Auth == nil || settings.Auth.User == "" {
		return settings.Address
	}
	return settings.Address + "|" + settings.Auth.User
}

// newV6DialObserver builds the wrapper for one proxied slot. A direct slot has
// no proxy and gets nil: it needs no verdict, because it dials on the host's
// own routes and keeps the group.
func newV6DialObserver(
	inner connect.DialContextFunction,
	proxySettings *connect.ProxySettings,
	platformUrlV6 string,
	platformUrlV4 string,
	platformUrl string,
	ledger *v6Ledger,
	onChange func(),
	now func() time.Time,
) *v6DialObserver {
	if inner == nil || proxySettings == nil || ledger == nil {
		return nil
	}
	host := urlHost(platformUrlV6)
	if host == "" {
		return nil
	}
	corroborating := map[string]bool{}
	for _, raw := range []string{platformUrlV4, platformUrl} {
		if h := urlHost(raw); h != "" && h != host {
			corroborating[h] = true
		}
	}
	if now == nil {
		now = time.Now
	}
	return &v6DialObserver{
		inner:              inner,
		host:               host,
		key:                v6LedgerKey(proxySettings),
		corroboratingHosts: corroborating,
		ledger:             ledger,
		onChange:           onChange,
		now:                now,
	}
}

// isV6Held reports whether err is the local refusal, for callers that want to
// distinguish it from a proxy error in diagnostics.
func isV6Held(err error) bool {
	return errors.Is(err, errV6Held) || (err != nil && strings.Contains(err.Error(), errV6Held.Error()))
}
