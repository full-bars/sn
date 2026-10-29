package miner

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// testHttpRequestExhausted mirrors connect's HttpRequestExhaustedError: same
// message, and an Unwrap() []error that keeps the observed causes. Its fields
// are unexported in connect, so the test builds the shape directly.
type testHttpRequestExhausted struct{ causes []error }

func (e *testHttpRequestExhausted) Error() string   { return "http request attempts exhausted" }
func (e *testHttpRequestExhausted) Unwrap() []error { return e.causes }

// socksRejection is the leaf the socks dialer returns when the proxy refuses
// the username/password, as seen in the operator log.
func socksRejection() error {
	return &net.OpError{Op: "socks connect", Net: "tcp", Err: errors.New("username/password authentication failed")}
}

// A proxy refusing our SOCKS credentials must be named as the cause, with the
// proxy address and obfuscated user, and never blamed on the identity. The
// cause sits under the exhausted-attempts error, as the attempt loop leaves it.
func TestProxyAuthRejectionNamesProxyCredentialNotIdentity(t *testing.T) {
	settings := &connect.ProxySettings{
		Network: "tcp",
		Address: "203.0.113.7:1080",
		Auth:    &proxy.Auth{User: "ab-proxy-user-yz", Password: "secret"},
	}
	exhausted := &testHttpRequestExhausted{causes: []error{socksRejection()}}

	got := proxyAuthRejectionError(settings, exhausted).Error()

	want := fmt.Sprintf("proxy %s rejected the proxy credentials: http request attempts exhausted", proxyKeyDisplay(settings.Key()))
	if got != want {
		t.Fatalf("proxyAuthRejectionError() = %q, want %q", got, want)
	}
	if !strings.Contains(got, "203.0.113.7:1080") {
		t.Fatalf("message %q does not name the proxy address", got)
	}
	if strings.Contains(got, "ab-proxy-user-yz") {
		t.Fatalf("message %q leaks the raw proxy user", got)
	}
	if strings.Contains(strings.ToLower(got), "identity") {
		t.Fatalf("message %q blames the identity for a proxy refusal", got)
	}
	if !proxyAuthRejected(exhausted) {
		t.Fatal("the exhausted-attempts error must still report as a proxy rejection")
	}
}

// Only a refusal is named as one. A proxy that cannot be dialed at all is not
// a credential problem, so its error passes through unchanged.
func TestProxyAuthRejectionLeavesOtherFailuresUnnamed(t *testing.T) {
	settings := &connect.ProxySettings{Network: "tcp", Address: "203.0.113.7:1080"}
	dialFailure := &net.OpError{Op: "socks connect", Net: "tcp", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}}
	exhausted := &testHttpRequestExhausted{causes: []error{dialFailure}}

	if proxyAuthRejected(exhausted) {
		t.Fatal("a dial failure to the proxy is not a credential rejection")
	}
	if got := proxyAuthRejectionError(settings, exhausted); got != error(exhausted) {
		t.Fatalf("proxyAuthRejectionError() rewrapped a non-rejection: %v", got)
	}
}

// A file-configured proxy has no eviction ceiling, so its give-up line must
// not print "of M". The line states the count and when the next attempt runs.
func TestProxyGiveUpLineOmitsDenominatorForFileProxy(t *testing.T) {
	swapOutageTracker(t, false)
	prevHistory := globalProxyFailureHistory
	globalProxyFailureHistory = &proxyFailureHistory{failures: map[string]int{}}
	t.Cleanup(func() { globalProxyFailureHistory = prevHistory })

	settings := &connect.ProxySettings{Network: "tcp", Address: "198.51.100.44:1080"}
	err := &testHttpRequestExhausted{causes: []error{socksRejection()}}

	var out string
	for i := 0; i < unprovenMaxAuthFailures; i++ {
		out = captureTlog(t, func() {
			noteProviderAuthFailure(settings, err)
		})
	}

	if !strings.Contains(out, "give-up 1 after 3 failed authentications; will retry in ") {
		t.Fatalf("give-up line = %q, want the no-denominator form", out)
	}
	if strings.Contains(out, " of ") {
		t.Fatalf("give-up line %q prints a ceiling that does not apply to a file proxy", out)
	}
}
