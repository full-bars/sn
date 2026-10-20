package miner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// importantLogf must reach both sinks the v33 provider wires: the ramlog (the
// miner's tlog -> proxyManagerLogWriter) and the disk critical log (critLog ->
// <home>/.urnetwork/events.log). The hook, when set, takes the line over
// completely, so tests can assert on the exact formatted line without either
// side effect (v33 asserts it from its oom-cap suite; the miner's copy of the
// seam needs its own test).
func TestImportantLogfWritesBothSinksAndHonorsTheHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	var ramlog strings.Builder
	oldWriter := proxyManagerLogWriter
	proxyManagerLogWriter = &ramlog
	t.Cleanup(func() { proxyManagerLogWriter = oldWriter })

	importantLogf("[test] two sinks %d\n", 7)

	if got := ramlog.String(); !strings.Contains(got, "[test] two sinks 7") {
		t.Fatalf("ramlog sink missing the line: %q", got)
	}
	b, err := os.ReadFile(filepath.Join(home, ".urnetwork", "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "[test] two sinks 7") {
		t.Fatalf("events.log sink missing the line: %q", string(b))
	}

	var hooked []string
	oldHook := importantLogHook
	importantLogHook = func(line string) { hooked = append(hooked, line) }
	t.Cleanup(func() { importantLogHook = oldHook })

	ramlog.Reset()
	importantLogf("[test] hooked %d", 9)
	if len(hooked) != 1 || hooked[0] != "[test] hooked 9" {
		t.Fatalf("hook got %q, want one exact line", hooked)
	}
	if ramlog.Len() != 0 {
		t.Fatalf("hook did not take the line over; ramlog got %q", ramlog.String())
	}
}
