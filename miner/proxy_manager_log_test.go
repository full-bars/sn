package miner

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// Every tab-style line carries exactly one stamp, and that includes the
// continuation lines of a multi-line message. The stream mixes glog lines
// (which stamp themselves), the diagnostics exporter, and these lines, so a
// missing or doubled stamp is as unreadable as none.
func TestTlogStampsEveryLineOnce(t *testing.T) {
	var buf bytes.Buffer
	oldWriter, oldClock := proxyManagerLogWriter, tabLogClock
	proxyManagerLogWriter = &buf
	tabLogClock = func() time.Time {
		return time.Date(2026, 10, 26, 17, 31, 52, 302000000, time.UTC)
	}
	defer func() {
		proxyManagerLogWriter, tabLogClock = oldWriter, oldClock
	}()

	tlog("[proxy][udp] slot=proxy:1.2.3.4:1081 udp relay works\n")
	got := buf.String()

	if !strings.HasPrefix(got, "I1026 17:31:52.302000 [proxy][udp] slot=proxy:1.2.3.4:1081") {
		t.Fatalf("line does not start with one stamp and the message: %q", got)
	}
	if n := strings.Count(got, "17:31:52.302000"); n != 1 {
		t.Fatalf("timestamp appears %d times in %q", n, got)
	}
	if n := strings.Count(got, "I1026 "); n != 1 {
		t.Fatalf("stamp prefix appears %d times in %q", n, got)
	}

	// A multi-line message stamps each line, and the capture helper strips all
	// of them, so an assertion on the message still sees just the message.
	buf.Reset()
	tlog("[proxy] first line\nsecond line\n")
	multiline := buf.String()
	if n := strings.Count(multiline, "I1026 17:31:52.302000 "); n != 2 {
		t.Fatalf("multi-line message has %d stamps, want 2: %q", n, multiline)
	}
	if stripped := stripTlogStamp(multiline); stripped != "[proxy] first line\nsecond line\n" {
		t.Fatalf("stripTlogStamp left %q", stripped)
	}
}
