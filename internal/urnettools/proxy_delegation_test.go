package urnettools

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProxyDelegation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub binary is POSIX-only")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-provider")
	logFile := filepath.Join(dir, "invocations.log")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %q\n", logFile)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	p := Provider{
		Unit:    "test-provider.service",
		User:    "test-user",
		Binary:  bin,
		Running: true,
	}

	origP, origS := discoverProcessesFn, discoverStoppedFn
	discoverProcessesFn = func() []Provider {
		return []Provider{p}
	}
	discoverStoppedFn = func([]Provider) []Provider { return nil }
	defer func() {
		discoverProcessesFn, discoverStoppedFn = origP, origS
	}()

	readLog := func() string {
		data, err := os.ReadFile(logFile)
		if err != nil && os.IsNotExist(err) {
			return ""
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	clearLog := func() {
		_ = os.Remove(logFile)
	}

	// 1. proxy summary runs provider binary with args "proxy summary" (and positionals passed through)
	clearLog()
	if err := cmdProxy([]string{"summary"}, false, false); err != nil {
		t.Fatalf("cmdProxy(summary): %v", err)
	}
	if got := strings.TrimSpace(readLog()); got != "proxy summary" {
		t.Errorf("proxy summary invoked fake binary with %q, want %q", got, "proxy summary")
	}

	clearLog()
	if err := cmdProxy([]string{"summary", "extra-arg"}, false, false); err != nil {
		t.Fatalf("cmdProxy(summary extra-arg): %v", err)
	}
	if got := strings.TrimSpace(readLog()); got != "proxy summary extra-arg" {
		t.Errorf("proxy summary with positionals invoked fake binary with %q, want %q", got, "proxy summary extra-arg")
	}

	// 2. proxy activity runs provider binary with args "proxy activity" (and positionals passed through)
	clearLog()
	if err := cmdProxy([]string{"activity"}, false, false); err != nil {
		t.Fatalf("cmdProxy(activity): %v", err)
	}
	if got := strings.TrimSpace(readLog()); got != "proxy activity" {
		t.Errorf("proxy activity invoked fake binary with %q, want %q", got, "proxy activity")
	}

	clearLog()
	if err := cmdProxy([]string{"activity", "extra-arg"}, false, false); err != nil {
		t.Fatalf("cmdProxy(activity extra-arg): %v", err)
	}
	if got := strings.TrimSpace(readLog()); got != "proxy activity extra-arg" {
		t.Errorf("proxy activity with positionals invoked fake binary with %q, want %q", got, "proxy activity extra-arg")
	}

	// 3. proxy exclude 1.2.3.4:1080 is delegated with its argument
	clearLog()
	if err := cmdProxy([]string{"exclude", "1.2.3.4:1080"}, false, false); err != nil {
		t.Fatalf("cmdProxy(exclude 1.2.3.4:1080): %v", err)
	}
	if got := strings.TrimSpace(readLog()); got != "proxy exclude 1.2.3.4:1080" {
		t.Errorf("proxy exclude invoked fake binary with %q, want %q", got, "proxy exclude 1.2.3.4:1080")
	}

	// 4. proxy exclude with dryRun=true does NOT execute the binary and prints dry-run notice
	clearLog()
	oldStdout := os.Stdout
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = wOut
	dryRunErr := cmdProxy([]string{"exclude", "1.2.3.4:1080"}, false, true)
	wOut.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, rOut)
	rOut.Close()

	if dryRunErr != nil {
		t.Fatalf("cmdProxy(exclude dryRun): %v", dryRunErr)
	}
	if got := readLog(); got != "" {
		t.Errorf("proxy exclude dry-run executed provider binary: %q, want empty", got)
	}
	wantNotice := "[dry-run] would run proxy exclude on test-provider.service\n"
	if buf.String() != wantNotice {
		t.Errorf("proxy exclude dry-run stdout = %q, want %q", buf.String(), wantNotice)
	}

	// 5. proxy summary --all is rejected with the single-provider error
	err = cmdProxy([]string{"summary", "--all"}, false, false)
	if err == nil {
		t.Fatal("proxy summary --all must return error")
	}
	if !strings.Contains(err.Error(), "operates on ONE provider") {
		t.Errorf("proxy summary --all error = %v, want single-provider error", err)
	}
}
