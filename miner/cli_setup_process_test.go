package miner

import (
	"os"
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

func TestSetupChildStopUsesKillOnWindows(t *testing.T) {
	process := &setupChildProcessRecorder{}
	if err := stopSetupChild(process, true); err != nil {
		t.Fatal(err)
	}
	if process.kills != 1 || len(process.signals) != 0 {
		t.Fatalf("stopSetupChild on Windows called Kill %d times and Signal %v, want one Kill and no Signal", process.kills, process.signals)
	}
}

func TestSetupChildStopUsesInterruptOnUnix(t *testing.T) {
	process := &setupChildProcessRecorder{}
	if err := stopSetupChild(process, false); err != nil {
		t.Fatal(err)
	}
	if process.kills != 0 || len(process.signals) != 1 || process.signals[0] != syscall.SIGINT {
		t.Fatalf("stopSetupChild on Unix called Kill %d times and Signal %v, want SIGINT only", process.kills, process.signals)
	}
}
