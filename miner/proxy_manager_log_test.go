package miner

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// A tab-style line carries exactly one timestamp. The stream mixes glog lines
// (which stamp themselves), the diagnostics exporter (which stamps nothing),
// and these lines, so a second stamp here would be as unreadable as none.
func TestTlogStampsTheLineOnce(t *testing.T) {
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

	if !strings.HasPrefix(got, "1026 17:31:52.302 [proxy][udp] slot=proxy:1.2.3.4:1081") {
		t.Fatalf("line does not start with one stamp and the message: %q", got)
	}
	if n := strings.Count(got, "17:31:52.302"); n != 1 {
		t.Fatalf("timestamp appears %d times in %q", n, got)
	}
	if n := strings.Count(got, "1026 "); n != 1 {
		t.Fatalf("date prefix appears %d times in %q", n, got)
	}
}
