package miner

// The probe is the only thing standing between a blackholed v6 route and an
// activation post the operator records but that can never be used, so what it
// decides is pinned here rather than inferred from a real socket: a reply that
// answers the query is usable and every way of not answering it is not.

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// probeTestTimeout is short enough that a silent peer fails the test quickly;
// the deadline is what ends the wait, never a sleep.
const probeTestTimeout = 50 * time.Millisecond

// ipv6ProbePipe runs probeIPv6Reply over a pipe whose far end reads the query
// and writes whatever reply builds from it, or stays silent when reply returns
// nil. The query that went out comes back so a test can read it.
func ipv6ProbePipe(t *testing.T, reply func(query []byte) []byte) (bool, string, []byte) {
	t.Helper()
	client, server := net.Pipe()
	sent := make(chan []byte, 1)
	go func() {
		defer server.Close()
		buf := make([]byte, ipv6ProbeReplyMax)
		n, err := server.Read(buf)
		if err != nil {
			return
		}
		query := append([]byte(nil), buf[:n]...)
		sent <- query
		if answer := reply(query); len(answer) != 0 {
			server.Write(answer)
			return
		}
		// Stay open and silent: a blackholed path drops the packet instead of
		// closing anything, so the probe has to wait out its own deadline.
		io.Copy(io.Discard, server)
	}()

	usable, reason := probeIPv6Reply(client, probeTestTimeout)
	select {
	case query := <-sent:
		return usable, reason, query
	case <-time.After(2 * time.Second):
		t.Fatal("the probe sent nothing")
		return false, "", nil
	}
}

// An answer that carries this query's id proves the path in both directions.
func TestIPv6ProbeAcceptsAReplyThatAnswersTheQuery(t *testing.T) {
	usable, reason, _ := ipv6ProbePipe(t, func(query []byte) []byte {
		answer := make([]byte, 12)
		copy(answer[0:2], query[0:2])
		answer[2] = 0x81
		binary.BigEndian.PutUint16(answer[6:8], 1)
		return answer
	})
	if !usable {
		t.Fatalf("a matching reply measured unusable: %q", reason)
	}
	if reason != "" {
		t.Fatalf("usable with reason %q, want none", reason)
	}
}

// The whole point of the change: a path that accepts the send and returns
// nothing is not usable, and the reason says so plainly.
func TestIPv6ProbeWithoutAnAnswerIsNotUsable(t *testing.T) {
	usable, reason, _ := ipv6ProbePipe(t, func(query []byte) []byte { return nil })
	if usable {
		t.Fatal("a path that returned nothing measured usable")
	}
	if !strings.Contains(reason, "query sent") || !strings.Contains(reason, "nothing came back") {
		t.Fatalf("reason = %q, want it to say the query was sent and nothing came back", reason)
	}
}

// A reply to some other query is not evidence about this one.
func TestIPv6ProbeWithAnotherQuerysReplyIsNotUsable(t *testing.T) {
	usable, reason, query := ipv6ProbePipe(t, func(query []byte) []byte {
		answer := make([]byte, 12)
		binary.BigEndian.PutUint16(answer[0:2], binary.BigEndian.Uint16(query[0:2])+1)
		answer[2] = 0x81
		return answer
	})
	if usable {
		t.Fatal("a reply with another query's id measured usable")
	}
	if !strings.Contains(reason, "does not answer") {
		t.Fatalf("reason = %q, want it to say the reply does not answer the query", reason)
	}
	if len(query) < 12 {
		t.Fatalf("the query was %d bytes, too short for a dns header", len(query))
	}
}

// A truncated answer cannot be checked, so it is not an answer.
func TestIPv6ProbeWithAShortReplyIsNotUsable(t *testing.T) {
	usable, reason, _ := ipv6ProbePipe(t, func(query []byte) []byte {
		return query[0:3]
	})
	if usable {
		t.Fatal("a 3-byte reply measured usable")
	}
	if !strings.Contains(reason, "too short") {
		t.Fatalf("reason = %q, want it to say the reply is too short", reason)
	}
}

// A path that is refused or gone is not usable either.
func TestIPv6ProbeOnAClosedPathIsNotUsable(t *testing.T) {
	client, server := net.Pipe()
	server.Close()
	if usable, reason := probeIPv6Reply(client, probeTestTimeout); usable || reason == "" {
		t.Fatalf("closed path: usable=%v reason=%q, want unusable with a reason", usable, reason)
	}
}

// The precheck decides before any syscall: a host with no global v6 address
// never spends a probe.
func TestIPv6ProbeWithoutAGlobalAddressShortCircuits(t *testing.T) {
	previous := ipv6GlobalAddressPresent
	ipv6GlobalAddressPresent = func() bool { return false }
	t.Cleanup(func() { ipv6GlobalAddressPresent = previous })

	usable, reason := probeIPv6Usable()
	if usable || reason != "no global ipv6 address" {
		t.Fatalf("usable=%v reason=%q, want unusable with the precheck's reason", usable, reason)
	}
}

// The probe asks a resolver a real question, so what it puts on the wire has
// to be a valid A query: a header with recursion desired, one question, and
// the name in length-prefixed labels followed by QTYPE A and QCLASS IN.
func TestIPv6ProbeQueryIsAWellFormedAQuery(t *testing.T) {
	const id = 0xbeef
	query := ipv6ProbeQuery(id)
	if len(query) < 12 {
		t.Fatalf("query is %d bytes, too short for a dns header", len(query))
	}
	if got := binary.BigEndian.Uint16(query[0:2]); got != id {
		t.Fatalf("id = %#x, want %#x", got, id)
	}
	if query[2]&0x80 != 0 {
		t.Fatal("the query has the response bit set")
	}
	if query[2]&0x01 == 0 {
		t.Fatal("recursion desired is clear")
	}
	if got := binary.BigEndian.Uint16(query[4:6]); got != 1 {
		t.Fatalf("question count = %d, want 1", got)
	}
	for _, offset := range []int{6, 8, 10} {
		if got := binary.BigEndian.Uint16(query[offset : offset+2]); got != 0 {
			t.Fatalf("record count at %d = %d, want 0", offset, got)
		}
	}

	var wantName []byte
	for _, label := range strings.Split(ipv6ProbeQueryName, ".") {
		wantName = append(wantName, byte(len(label)))
		wantName = append(wantName, label...)
	}
	wantName = append(wantName, 0)
	if got := string(query[12 : 12+len(wantName)]); got != string(wantName) {
		t.Fatalf("question name = %q, want %q", got, wantName)
	}
	offset := 12 + len(wantName)
	if got := binary.BigEndian.Uint16(query[offset : offset+2]); got != 1 {
		t.Fatalf("qtype = %d, want 1 (A)", got)
	}
	if got := binary.BigEndian.Uint16(query[offset+2 : offset+4]); got != 1 {
		t.Fatalf("qclass = %d, want 1 (IN)", got)
	}
	if len(query) != offset+4 {
		t.Fatalf("query is %d bytes, want %d", len(query), offset+4)
	}
}
