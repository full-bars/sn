package miner

// cmd_logs.go ports `provider logs [-n <lines>]` from the 3.23-fix v33
// provider (provider/main.go providerLogs/readSHMLog; the sn relay carries the
// import-adapted copy in provider/cmd_main.go). The command reads the shared
// RAM log and then tails it, so an operator can watch a provider running with
// URNETWORK_RAMLOGS=1.
//
// The RAM-log WRITER is provider-side logging and is not part of this port: the
// miner's own logger does not redirect to /dev/shm yet. The command therefore
// behaves exactly like v33 for a host whose log file exists (a 3.23-fix
// provider, or the relay), and reports the same missing-file fatal otherwise.

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
)

// minerSHMLogPath is the v33 shared-memory log path (provider/shmlog_linux.go;
// the fallback for other platforms uses the same name).
const minerSHMLogPath = "/dev/shm/urnetwork.log"

// minerCmdFatal is the miner's port of the provider's shmLogFatal: the message
// on stderr (and appended to the RAM log when one exists) and the v33 exit
// code. One-shot CLI subcommands own the process, so exiting here matches v33.
func minerCmdFatal(code int, format string, args ...any) {
	msg := fmt.Sprintf("FATAL [exit %d]: %s\n", code, fmt.Sprintf(format, args...))
	if f, err := os.OpenFile(minerSHMLogPath, os.O_WRONLY|os.O_APPEND, 0); err == nil {
		f.Write([]byte(msg))
		f.Sync()
		f.Close()
	}
	os.Stderr.Write([]byte(msg))
	os.Exit(code)
}

// readSHMLog reads the last n lines from the shared-memory log file.
// If n <= 0, the entire file is returned.
func readSHMLog(path string, n int) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if n <= 0 {
		return string(b), nil
	}
	s := strings.TrimRight(string(b), "\n")
	lines := strings.Split(s, "\n")
	if n > len(lines) {
		n = len(lines)
	}
	return strings.Join(lines[len(lines)-n:], "\n") + "\n", nil
}

// providerLogs implements `provider logs [-n <lines>]`: tails the RAM log.
func providerLogs(opts docopt.Opts) {
	n, _ := opts.Int("-n")
	out, err := readSHMLog(minerSHMLogPath, n)
	if err != nil {
		minerCmdFatal(40, "no ramlogs found at %s — is URNETWORK_RAMLOGS=1 set?", minerSHMLogPath)
	}
	fmt.Print(out)

	// Tail: follow the file from current position.
	f, err := os.Open(minerSHMLogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: could not open log for tailing: %v\n", err)
		return
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		fmt.Fprintf(os.Stderr, "error: seek failed: %v\n", err)
		return
	}

	buf := make([]byte, 4096)
	for {
		nr, readErr := f.Read(buf)
		if nr > 0 {
			os.Stdout.Write(buf[:nr])
		}
		if readErr != nil && readErr != io.EOF {
			f.Close()
			fmt.Fprintf(os.Stderr, "error: read failed: %v\n", readErr)
			return
		}
		if readErr == io.EOF {
			// Detect ramlogs wrap: if the file shrunk behind our position, reopen from start.
			if pos, _ := f.Seek(0, io.SeekCurrent); pos > 0 {
				if fi, statErr := f.Stat(); statErr == nil && fi.Size() < pos {
					f.Close()
					newF, openErr := os.Open(minerSHMLogPath)
					if openErr != nil {
						fmt.Fprintf(os.Stderr, "error: could not reopen log after wrap: %v\n", openErr)
						return
					}
					f = newF
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}
