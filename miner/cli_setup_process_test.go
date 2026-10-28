package miner

import (
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

type setupChildProcessRecorder struct {
	signals []os.Signal
	kills   int
}

func (p *setupChildProcessRecorder) Signal(signal os.Signal) error {
	p.signals = append(p.signals, signal)
	return nil
}

func (p *setupChildProcessRecorder) Kill() error {
	p.kills++
	return nil
}

// The platform is chosen inside stopSetupChild, so each branch only runs on
// its own host. The guard test below covers the call sites on every host.
func TestSetupChildStopUsesKillOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Kill branch runs only on Windows")
	}
	process := &setupChildProcessRecorder{}
	if err := stopSetupChild(process); err != nil {
		t.Fatal(err)
	}
	if process.kills != 1 || len(process.signals) != 0 {
		t.Fatalf("stopSetupChild on Windows called Kill %d times and Signal %v, want one Kill and no Signal", process.kills, process.signals)
	}
}

func TestSetupChildStopUsesInterruptOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT branch does not run on Windows")
	}
	process := &setupChildProcessRecorder{}
	if err := stopSetupChild(process); err != nil {
		t.Fatal(err)
	}
	if process.kills != 0 || len(process.signals) != 1 || process.signals[0] != syscall.SIGINT {
		t.Fatalf("stopSetupChild on Unix called Kill %d times and Signal %v, want SIGINT only", process.kills, process.signals)
	}
}

// TestSetupCallSitesDoNotBypassStopSetupChild pins the regression: a call
// site that signals command.Process directly skips the Windows Kill path.
func TestSetupCallSitesDoNotBypassStopSetupChild(t *testing.T) {
	src, err := os.ReadFile("cli_setup.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "Process.Signal(syscall.SIGINT)") {
		t.Fatal("cli_setup.go signals the setup child directly; call stopSetupChild instead so Windows uses Kill")
	}
}
