package urnettools

import (
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/internal/tui/tcellui"
)

var (
	// User left empty so crossUserElevationNeeded returns false and top reaches runTop.
	stoppedSec = Provider{Unit: "urnetwork-sec.service", StateDir: "/home/sec/.urnetwork"}
	runningOne = Provider{Unit: "urnetwork.service", StateDir: "/home/me/.urnetwork", Running: true, PID: 42}
)

func TestProvidersForTopHidesStoppedOnes(t *testing.T) {
	shown, hidden, err := providersForTop([]Provider{stoppedSec, runningOne}, Target{}, false)
	if err != nil || hidden != 1 || len(shown) != 1 || shown[0].Unit != "urnetwork.service" {
		t.Fatalf("shown=%v hidden=%d err=%v, want the running provider and one hidden", shown, hidden, err)
	}
}

func TestProvidersForTopKeepsEverythingForAllOrAnExplicitTarget(t *testing.T) {
	both := []Provider{stoppedSec, runningOne}
	if shown, hidden, err := providersForTop(both, Target{}, true); err != nil || hidden != 0 || len(shown) != 2 {
		t.Fatalf("--all: shown=%d hidden=%d err=%v, want both", len(shown), hidden, err)
	}
	if shown, hidden, err := providersForTop(both, Target{Unit: "urnetwork-sec.service"}, false); err != nil || hidden != 0 || len(shown) != 2 {
		t.Fatalf("a named provider: shown=%d hidden=%d err=%v, want the list left alone", len(shown), hidden, err)
	}
}

func TestProvidersForTopSaysWhatToDoWhenNothingIsRunning(t *testing.T) {
	_, hidden, err := providersForTop([]Provider{stoppedSec}, Target{}, false)
	if err == nil || hidden != 1 {
		t.Fatalf("hidden=%d err=%v, want an error", hidden, err)
	}
	for _, want := range []string{"no running provider found", "urnetwork-sec.service", "--all", "--unit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// nothing found at all is not this error: discovery reports that itself
	if shown, hidden, err := providersForTop(nil, Target{}, false); err != nil || hidden != 0 || len(shown) != 0 {
		t.Fatalf("no providers: shown=%v hidden=%d err=%v, want quietly nothing", shown, hidden, err)
	}
}

func TestStripAllFlagAndTheNote(t *testing.T) {
	all, rest := stripAllFlag([]string{"--unit", "x", "--all", "--interval", "1s"})
	if !all || strings.Join(rest, " ") != "--unit x --interval 1s" {
		t.Fatalf("all=%v rest=%v", all, rest)
	}
	if all, _ := stripAllFlag([]string{"--unit", "x"}); all {
		t.Fatal("--all reported when absent")
	}
	if got := stoppedHiddenNote(1); got != "1 stopped provider hidden (--all shows it)" {
		t.Errorf("note(1) = %q", got)
	}
	if got := stoppedHiddenNote(3); got != "3 stopped providers hidden (--all shows them)" {
		t.Errorf("note(3) = %q", got)
	}
}

func finishTop(t *testing.T, scr *frameScreen, result chan error) {
	t.Helper()
	scr.events <- tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyCtrlC}
	select {
	case <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("top did not return after ctrl-c")
	}
}

// The screen from the report: a stopped provider listed first must not be where
// top opens, and its name must not be on any frame.
func TestCmdTopOpensOnTheRunningProviderNotAStoppedOne(t *testing.T) {
	scr := newFrameScreenAt(80, 24)
	stubTopDiscovery(t, scr, func() []Provider { return []Provider{stoppedSec, runningOne} })
	result := make(chan error, 1)
	go func() { result <- cmdTop(nil) }()

	frames := scr.waitForFrame(t, "urnetwork.service")
	for _, f := range frames {
		if strings.Contains(f, "urnetwork-sec.service") {
			t.Fatalf("a stopped provider was drawn:\n%s", f)
		}
	}
	finishTop(t, scr, result)
}

func TestCmdTopAllShowsStoppedProvidersToo(t *testing.T) {
	scr := newFrameScreenAt(80, 24)
	stubTopDiscovery(t, scr, func() []Provider { return []Provider{stoppedSec, runningOne} })
	result := make(chan error, 1)
	go func() { result <- cmdTop([]string{"--all"}) }()

	scr.waitForFrame(t, "[1/2]")
	finishTop(t, scr, result)
}

func TestCmdTopWithOnlyStoppedProvidersSaysSo(t *testing.T) {
	scr := newFrameScreenAt(80, 24)
	stubTopDiscovery(t, scr, func() []Provider { return []Provider{stoppedSec} })
	result := make(chan error, 1)
	go func() { result <- cmdTop(nil) }()

	scr.waitForFrame(t, "no running provider found")
	scr.events <- tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyEnter}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "urnetwork-sec.service") {
			t.Fatalf("err = %v, want the stopped provider named", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("top did not return")
	}
}

func TestCmdTopStillOpensAStoppedProviderThatWasNamed(t *testing.T) {
	scr := newFrameScreenAt(80, 24)
	stubTopDiscovery(t, scr, func() []Provider { return []Provider{stoppedSec, runningOne} })
	result := make(chan error, 1)
	go func() { result <- cmdTop([]string{"--unit", "urnetwork-sec.service"}) }()

	scr.waitForFrame(t, "urnetwork-sec.service")
	finishTop(t, scr, result)
}
