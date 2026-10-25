package miner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestClassifyV6Probe(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}
	ioTimeout := &net.OpError{Op: "read", Net: "tcp", Err: timeoutErr{}}
	cases := []struct {
		name  string
		err   error
		reply int
		want  v6Verdict
	}{
		{"success", nil, 0x00, v6Yes},
		{"success reply but stream error after", io.ErrUnexpectedEOF, 0x00, v6Unknown},
		{"nil error, no reply", nil, socksNoReply, v6Unknown},
		{"host unreachable", errors.New("socks connect tcp6: host unreachable"), 0x04, v6No},
		{"addr type not supported", errors.New("socks connect: address type not supported"), 0x08, v6No},
		{"not allowed by ruleset", errors.New("socks connect: connection not allowed by ruleset"), 0x02, v6No},
		{"network unreachable", errors.New("socks connect: network unreachable"), 0x03, v6No},
		{"general failure", errors.New("general SOCKS server failure"), 0x01, v6Unknown},
		{"target refused", errors.New("connection refused"), 0x05, v6Unknown},
		{"ttl expired", errors.New("ttl expired"), 0x06, v6Unknown},
		{"command not supported", errors.New("command not supported"), 0x07, v6Unknown},
		{"undefined reply code", errors.New("weird"), 0x09, v6Unknown},
		{"reply code beats a context error", context.Canceled, 0x04, v6No},
		{"EOF", io.EOF, socksNoReply, v6No},
		{"unexpected EOF", io.ErrUnexpectedEOF, socksNoReply, v6No},
		{"wrapped unexpected EOF", fmt.Errorf("socks connect tcp6 1.2.3.4:1080->connect-v6.bringyour.com:443: %w", io.ErrUnexpectedEOF), socksNoReply, v6No},
		{"timeout net.Error", ioTimeout, socksNoReply, v6Unknown},
		{"os deadline", fmt.Errorf("read: %w", os.ErrDeadlineExceeded), socksNoReply, v6Unknown},
		{"context deadline", fmt.Errorf("dial: %w", context.DeadlineExceeded), socksNoReply, v6Unknown},
		{"context canceled", context.Canceled, socksNoReply, v6Unknown},
		{"EOF caused by our own cancel", fmt.Errorf("%w: %w", context.Canceled, io.EOF), socksNoReply, v6Unknown},
		{"proxy connection refused", refused, socksNoReply, v6Unknown},
		{"unrecognized error", errors.New("auth failed"), socksNoReply, v6Unknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyV6Probe(c.err, c.reply); got != c.want {
				t.Fatalf("classifyV6Probe(%v, %#x) = %s, want %s", c.err, c.reply, got, c.want)
			}
		})
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestLedger() (*v6Ledger, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	return newV6Ledger(c.now), c
}

const px = "10.0.0.1:1080"

func TestLedgerMissingIsUnknown(t *testing.T) {
	l, _ := newTestLedger()
	if got := l.Verdict(px); got != v6Unknown {
		t.Fatalf("got %s", got)
	}
}

func TestLedgerYesExpiryBoundary(t *testing.T) {
	l, c := newTestLedger()
	l.Record(px, v6Yes)
	c.advance(v6YesTTL - time.Nanosecond)
	if got := l.Verdict(px); got != v6Yes {
		t.Fatalf("just before 6h: %s", got)
	}
	c.advance(time.Nanosecond)
	if got := l.Verdict(px); got != v6Unknown {
		t.Fatalf("at exactly 6h: %s", got)
	}
}

func TestLedgerNoExpiryBoundary(t *testing.T) {
	l, c := newTestLedger()
	l.Record(px, v6No)
	c.advance(v6NoTTL - time.Nanosecond)
	if got := l.Verdict(px); got != v6No {
		t.Fatalf("just before 15m: %s", got)
	}
	c.advance(time.Nanosecond)
	if got := l.Verdict(px); got != v6Unknown {
		t.Fatalf("at exactly 15m: %s", got)
	}
}

func TestLedgerNegativeAgeIsNotTrusted(t *testing.T) {
	l, c := newTestLedger()
	l.Record(px, v6Yes)
	c.advance(-time.Second) // clock stepped back
	if got := l.Verdict(px); got != v6Unknown {
		t.Fatalf("got %s", got)
	}
}

func TestLedgerTransitions(t *testing.T) {
	// from -> recorded -> expected effective verdict right after.
	for _, from := range []v6Verdict{v6Unknown, v6Yes, v6No} {
		for _, to := range []v6Verdict{v6Unknown, v6Yes, v6No} {
			t.Run(from.String()+"->"+to.String(), func(t *testing.T) {
				l, c := newTestLedger()
				l.Record(px, from)
				c.advance(time.Minute)
				l.Record(px, to)
				want := to
				if to == v6Unknown && from != v6Unknown {
					want = from // unknown never clobbers a live verdict
				}
				if got := l.Verdict(px); got != want {
					t.Fatalf("got %s, want %s", got, want)
				}
			})
		}
	}
}

func TestLedgerUnknownStampsExpiredEntry(t *testing.T) {
	l, c := newTestLedger()
	l.Record(px, v6No)
	c.advance(v6NoTTL)
	l.Record(px, v6Unknown)
	if got := l.entries[px]; got.Verdict != v6Unknown || !got.CheckedAt.Equal(c.t) {
		t.Fatalf("entry %+v", got)
	}
}

func TestLedgerRecordRestartsWindow(t *testing.T) {
	l, c := newTestLedger()
	l.Record(px, v6Yes)
	c.advance(v6YesTTL - time.Minute)
	l.Record(px, v6Yes)
	c.advance(time.Hour)
	if got := l.Verdict(px); got != v6Yes {
		t.Fatalf("got %s", got)
	}
}

func TestLedgerThreeFailuresFlipYesToNo(t *testing.T) {
	l, c := newTestLedger()
	l.Record(px, v6Yes)
	c.advance(time.Hour)
	for i := 1; i < v6FlipAfter; i++ {
		if l.ServiceFailure(px) {
			t.Fatalf("flipped after %d", i)
		}
		if got := l.Verdict(px); got != v6Yes {
			t.Fatalf("after %d failures: %s", i, got)
		}
	}
	if !l.ServiceFailure(px) {
		t.Fatal("no flip on third failure")
	}
	if got := l.Verdict(px); got != v6No {
		t.Fatalf("got %s", got)
	}
	// The no window starts at the flip, not at the original probe.
	c.advance(v6NoTTL - time.Nanosecond)
	if got := l.Verdict(px); got != v6No {
		t.Fatalf("just before flip+15m: %s", got)
	}
	c.advance(time.Nanosecond)
	if got := l.Verdict(px); got != v6Unknown {
		t.Fatalf("at flip+15m: %s", got)
	}
}

func TestLedgerSuccessResetsFailureRun(t *testing.T) {
	l, _ := newTestLedger()
	l.Record(px, v6Yes)
	l.ServiceFailure(px)
	l.ServiceFailure(px)
	l.ServiceSuccess(px)
	l.ServiceFailure(px)
	if l.ServiceFailure(px) {
		t.Fatal("run was not reset")
	}
	if got := l.Verdict(px); got != v6Yes {
		t.Fatalf("got %s", got)
	}
}

func TestLedgerSuccessDoesNotExtendTrust(t *testing.T) {
	l, c := newTestLedger()
	l.Record(px, v6Yes)
	c.advance(v6YesTTL - time.Minute)
	l.ServiceSuccess(px)
	c.advance(time.Minute)
	if got := l.Verdict(px); got != v6Unknown {
		t.Fatalf("got %s", got)
	}
}

func TestLedgerFailureIgnoredUnlessLiveYes(t *testing.T) {
	l, c := newTestLedger()
	for i := 0; i < 5; i++ {
		if l.ServiceFailure("absent") {
			t.Fatal("flipped an absent entry")
		}
	}
	l.Record(px, v6No)
	for i := 0; i < 5; i++ {
		l.ServiceFailure(px)
	}
	if got := l.entries[px]; got.Verdict != v6No || got.Failures != 0 {
		t.Fatalf("no entry changed: %+v", got)
	}
	l.Record(px, v6Yes)
	c.advance(v6YesTTL)
	for i := 0; i < 5; i++ {
		if l.ServiceFailure(px) {
			t.Fatal("flipped an expired yes")
		}
	}
	if got := l.entries[px]; got.Verdict != v6Yes || got.Failures != 0 {
		t.Fatalf("expired yes counted failures: %+v", got)
	}
}

func TestLedgerFreshProbeResetsFailures(t *testing.T) {
	l, _ := newTestLedger()
	l.Record(px, v6Yes)
	l.ServiceFailure(px)
	l.ServiceFailure(px)
	l.Record(px, v6Yes)
	if l.ServiceFailure(px) {
		t.Fatal("failure count survived a fresh probe")
	}
}

func TestLedgerKeysAreIndependent(t *testing.T) {
	l, _ := newTestLedger()
	l.Record("a:1", v6Yes)
	l.Record("b:1", v6No)
	if l.Verdict("a:1") != v6Yes || l.Verdict("b:1") != v6No || l.Verdict("c:1") != v6Unknown {
		t.Fatal("keys leaked")
	}
}

func TestLedgerJSONRoundTrip(t *testing.T) {
	l, c := newTestLedger()
	l.Record("a:1", v6Yes)
	l.ServiceFailure("a:1")
	l.Record("b:1", v6No)
	l.Record("c:1", v6Unknown)
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"version":1`, `"verdict":"yes"`, `"verdict":"no"`, `"verdict":"unknown"`, `"failures":1`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("%s missing from %s", want, b)
		}
	}
	l2 := newV6Ledger(c.now)
	if err := json.Unmarshal(b, l2); err != nil {
		t.Fatal(err)
	}
	if l2.Verdict("a:1") != v6Yes || l2.Verdict("b:1") != v6No || l2.Verdict("c:1") != v6Unknown {
		t.Fatalf("verdicts changed across round trip: %s", b)
	}
	// The failure count survived: two more failures flip it.
	l2.ServiceFailure("a:1")
	if !l2.ServiceFailure("a:1") {
		t.Fatal("failure count lost across round trip")
	}
	// Expiry still runs off the persisted CheckedAt.
	l3 := newV6Ledger(c.now)
	if err := json.Unmarshal(b, l3); err != nil {
		t.Fatal(err)
	}
	c.advance(v6NoTTL)
	if l3.Verdict("b:1") != v6Unknown || l3.Verdict("a:1") != v6Yes {
		t.Fatal("expiry wrong after reload")
	}
}

func TestLedgerJSONRejectsBadInput(t *testing.T) {
	for name, in := range map[string]string{
		"bad verdict": `{"version":1,"entries":{"a:1":{"verdict":"maybe","checked_at":"2026-10-25T12:00:00Z"}}}`,
		"bad version": `{"version":2,"entries":{}}`,
		"not json":    `{`,
	} {
		l, _ := newTestLedger()
		l.Record(px, v6Yes)
		if err := json.Unmarshal([]byte(in), l); err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if l.Verdict(px) != v6Yes {
			t.Fatalf("%s: a rejected load changed the ledger", name)
		}
	}
	l, _ := newTestLedger()
	if err := json.Unmarshal([]byte(`{"version":1}`), l); err != nil {
		t.Fatal(err)
	}
	l.Record(px, v6Yes) // a missing entries map must not leave a nil map
}

func TestLedgerNilClockDefaultsToTimeNow(t *testing.T) {
	l := newV6Ledger(nil)
	l.Record(px, v6Yes)
	if l.Verdict(px) != v6Yes {
		t.Fatal("real clock")
	}
}

func TestLedgerConcurrent(t *testing.T) {
	l, _ := newTestLedger()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := fmt.Sprintf("p%d:1", i%3)
			for j := 0; j < 200; j++ {
				l.Record(k, v6Yes)
				l.ServiceFailure(k)
				l.ServiceSuccess(k)
				l.Verdict(k)
				_, _ = json.Marshal(l)
			}
		}(i)
	}
	wg.Wait()
}
