package urnettools

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestProviderSubcommandStdinWiring guards the interactive-confirm regression:
// `proxy remove-dead` (and other delegated confirm subcommands) were run with
// stdin unwired, so the provider's confirm() read EOF and every prompt
// instantly resolved to "n" — "Remove N dead proxies? [y/N] Nothing to
// remove." with the user's "y" falling through to the shell.
//
// Coverage:
//   - paste:    stdin always wired (reads proxy list from stdin, even piped)
//   - confirm subcommand + interactive stdin: stdin wired (TTY)
//   - confirm subcommand + non-interactive stdin: stdin NOT wired (cron/pipe
//     must see EOF and refuse by default)
func TestProviderSubcommandStdinWiring(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script stub binary is POSIX-only")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-provider")
	// The fake provider reports whether it can read a line from stdin:
	// "STDIN:line" when it read data, "STDIN:eof" otherwise.
	script := "#!/bin/sh\n" +
		"if read -r line </dev/stdin 2>/dev/null; then echo \"STDIN:$line\"; else echo STDIN:eof; fi\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := Provider{Binary: bin, Unit: "test.service"}

	// Run the child with a stdin pipe we feed concurrently: providerSubcommand
	// runs synchronously, so the answer must be written while the child is
	// blocked reading, not after it returns.
	runChild := func(args ...string) (string, error) {
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		oldStdin := os.Stdin
		os.Stdin = pr
		oldStdout := os.Stdout
		rOut, wOut, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = wOut

		done := make(chan struct{})
		go func() {
			pw.WriteString("y\n")
			pw.Close()
			close(done)
		}()

		err = providerSubcommand(p, args...)
		wOut.Close()
		os.Stdout = oldStdout
		os.Stdin = oldStdin
		pr.Close()
		<-done

		buf := make([]byte, 4096)
		n, _ := rOut.Read(buf)
		return string(buf[:n]), err
	}

	sendStdin := func(t *testing.T, interactive bool, args ...string) string {
		t.Helper()
		told := stdinIsInteractiveOverride
		stdinIsInteractiveOverride = func() bool { return interactive }
		defer func() { stdinIsInteractiveOverride = told }()
		out, err := runChild(args...)
		if err != nil {
			t.Fatalf("providerSubcommand(%v) = %v, want nil", args, err)
		}
		return out
	}

	t.Run("paste always wired even when non-interactive", func(t *testing.T) {
		out := sendStdin(t, false, "proxy", "paste")
		if !strings.Contains(out, "STDIN:y") {
			t.Fatalf("paste child saw %q, want it to read stdin (STDIN:y)", out)
		}
	})

	t.Run("remove-dead wired when stdin is interactive", func(t *testing.T) {
		out := sendStdin(t, true, "proxy", "remove-dead")
		if !strings.Contains(out, "STDIN:y") {
			t.Fatalf("interactive remove-dead child saw %q, want STDIN:y (confirm must be answerable)", out)
		}
	})

	t.Run("remove-dead NOT wired when stdin is non-interactive", func(t *testing.T) {
		out := sendStdin(t, false, "proxy", "remove-dead")
		if !strings.Contains(out, "STDIN:eof") {
			t.Fatalf("non-interactive remove-dead child saw %q, want STDIN:eof (must refuse by default)", out)
		}
	})
}
