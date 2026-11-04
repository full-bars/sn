package urnettools

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/internal/tui"
	"github.com/urfoundation/sn/internal/tui/tcellui"
)

// cutShortSeen marks the flag the way a timed-out pass does and returns the
// providers it still found. The flag is cleared when the test ends.
func cutShortSeen(t *testing.T, found []Provider) []Provider {
	t.Helper()
	t.Cleanup(func() { systemdDiscoveryCutShort = false })
	systemdDiscoveryCutShort = true
	return found
}

const cutShortWant = "ran out of budget"

// A cut pass that still found a provider must say so. An empty list already
// carries the flag in its error, so only a non-empty result proves the gap.
func TestProvidersWarnsOnCutPassWithProviders(t *testing.T) {
	origP, origS, origPriv := discoverProcessesFn, discoverStoppedFn, isPrivileged
	t.Cleanup(func() { discoverProcessesFn, discoverStoppedFn, isPrivileged = origP, origS, origPriv })
	isPrivileged = func() bool { return true }
	discoverProcessesFn = func() []Provider {
		return []Provider{{Unit: "urnetwork.service", User: "u", StateDir: t.TempDir(), PID: 4242, Running: true}}
	}
	discoverStoppedFn = func([]Provider) []Provider { cutShortSeen(t, nil); return nil }

	var err error
	stderr := captureStderr(t, func() {
		captureStdout(t, func() { err = cmdProviders(nil) })
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, cutShortWant) || !strings.Contains(stderr, "(1 found)") {
		t.Fatalf("providers must warn on stderr with the count, got %q", stderr)
	}
}

func TestProvidersSilentWhenPassWasNotCut(t *testing.T) {
	origP, origS, origPriv := discoverProcessesFn, discoverStoppedFn, isPrivileged
	t.Cleanup(func() { discoverProcessesFn, discoverStoppedFn, isPrivileged = origP, origS, origPriv })
	isPrivileged = func() bool { return true }
	discoverProcessesFn = func() []Provider {
		return []Provider{{Unit: "urnetwork.service", User: "u", StateDir: t.TempDir(), PID: 4242, Running: true}}
	}
	discoverStoppedFn = func([]Provider) []Provider { return nil }

	stderr := captureStderr(t, func() {
		captureStdout(t, func() { _ = cmdProviders(nil) })
	})
	if strings.Contains(stderr, cutShortWant) {
		t.Fatalf("a complete pass must not warn, got %q", stderr)
	}
}

func TestStatusWarnsOnCutPassWithProviders(t *testing.T) {
	p := Provider{Unit: "a.service", User: "u", StateDir: t.TempDir(), Running: true}
	stubStatus(t, []Provider{p}, nil, true)
	discoverStatusFn = func() []Provider { return cutShortSeen(t, []Provider{p}) }

	stderr := captureStderr(t, func() {
		captureStdout(t, func() { _ = cmdStatus(nil) })
	})
	if !strings.Contains(stderr, cutShortWant) || !strings.Contains(stderr, "(1 found)") {
		t.Fatalf("status must warn on stderr with the count, got %q", stderr)
	}
}

// --json keeps stderr and the document clean for scripts: the signal is a
// field, present only when the pass was cut.
func TestStatusJSONCarriesCutShortField(t *testing.T) {
	p := Provider{Unit: "a.service", User: "u", StateDir: t.TempDir(), Running: true}
	snaps := map[string]json.RawMessage{"a.service": fixtureRaw(t, "node_snapshot_v1.json")}
	run := func(cut bool) map[string]any {
		stubStatus(t, []Provider{p}, snaps, true)
		systemdDiscoveryCutShort = false
		discoverStatusFn = func() []Provider {
			if cut {
				return cutShortSeen(t, []Provider{p})
			}
			return []Provider{p}
		}
		var err error
		out := captureStdout(t, func() { err = cmdStatus([]string{"--json"}) })
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("stdout is not one JSON object: %v\n%s", err, out)
		}
		return doc
	}
	if got := run(true)["discovery_cut_short"]; got != true {
		t.Fatalf("cut pass: discovery_cut_short = %v, want true", got)
	}
	if _, present := run(false)["discovery_cut_short"]; present {
		t.Fatal("a complete pass must not add the field")
	}
}

// top shows the warning on its footer row while leaving every other row, and so
// every column, where it was.
func TestTopFooterWarnsWithoutShiftingTheTable(t *testing.T) {
	render := func(note string) []string {
		clock := &fakeClock{t: topBase}
		m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
		m.apply(0, topFlowing(t), nil)
		m.discoveryNote = note
		b := tui.New(120, 40)
		m.render(b)
		return strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	}
	plain := render("")
	warned := render(discoveryCutShortWarning(1))
	if len(plain) != len(warned) {
		t.Fatalf("row count changed: %d -> %d", len(plain), len(warned))
	}
	for i := 0; i < len(plain)-1; i++ {
		if plain[i] != warned[i] {
			t.Fatalf("row %d shifted:\n%q\n%q", i, plain[i], warned[i])
		}
	}
	last := warned[len(warned)-1]
	if !strings.Contains(last, cutShortWant) || !strings.Contains(last, "(1 found)") || !strings.Contains(last, "q quit") {
		t.Fatalf("footer must carry the warning and keep the keys, got %q", last)
	}
	if strings.Contains(plain[len(plain)-1], cutShortWant) {
		t.Fatal("no warning expected without a note")
	}
}

// The warning must remain complete in both compact and full layouts, even
// when it takes more than one row.
func TestTopDiscoveryWarningFitsLayouts(t *testing.T) {
	for _, size := range []struct{ w, h int }{{60, 10}, {40, 8}, {72, 20}, {120, 40}} {
		m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
		m.discoveryNote = discoveryCutShortWarning(1)
		b := tui.New(size.w, size.h)
		m.render(b)
		var text []string
		for _, line := range strings.Split(b.String(), "\n") {
			text = append(text, strings.Trim(line, " │"))
		}
		if !strings.Contains(strings.Join(text, " "), m.discoveryNote) {
			t.Errorf("%dx%d: warning missing or truncated:\n%s", size.w, size.h, b.String())
		}
		if topMin.FitsRect(b.Rect()) {
			if !strings.Contains(b.String(), "- 1s +") {
				t.Errorf("%dx%d: key line missing or truncated:\n%s", size.w, size.h, b.String())
			}
		}
		if size.w == 40 && size.h == 8 {
			if !strings.Contains(b.String(), "q quit") {
				t.Errorf("%dx%d: quit hint missing:\n%s", size.w, size.h, b.String())
			}
		}
	}
}

// End to end: a cut pass with one provider puts the warning on the screen.
func TestCmdTopShowsWarningOnCutPassWithProviders(t *testing.T) {
	scr := newFrameScreen()
	stubTopDiscovery(t, scr, func() []Provider {
		return cutShortSeen(t, []Provider{{Unit: "a", Running: true}})
	})
	result := make(chan error, 1)
	go func() { result <- cmdTop(nil) }()

	scr.waitForFrame(t, cutShortWant)
	scr.events <- tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyCtrlC}
	select {
	case <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("top did not return after ctrl-c")
	}
}

// A cut pass must not return half-built entries as normal rows; restart, logs
// and set must refuse an entry with no unit rather than silently taking the
// unitless path.
func TestRestartLogsSetRefuseUnitlessEntryOnCutPass(t *testing.T) {
	dir := t.TempDir()
	p := Provider{User: currentUserName(), StateDir: dir, PID: 1234, Running: true}
	stubDiscovery(t, []Provider{p}, nil)
	discoverStoppedFn = func(running []Provider) []Provider {
		cutShortSeen(t, nil)
		return nil
	}
	discoverSystemdFn = func() []Provider {
		cutShortSeen(t, nil)
		return []Provider{p}
	}
	t.Cleanup(func() { systemdDiscoveryCutShort = false })

	// cmdRestart refuses
	errRestart := cmdRestart(nil, false, true)
	if errRestart == nil || !strings.Contains(errRestart.Error(), "ran out of budget") {
		t.Fatalf("cmdRestart must refuse unitless provider on cut pass, got: %v", errRestart)
	}

	// cmdLogs refuses where a journal exists. On Windows there is no journal at
	// all, so the command prints its not-supported notice and succeeds; a cut
	// pass does not change that.
	errLogs := cmdLogs(nil)
	if runtime.GOOS == "windows" {
		if errLogs != nil {
			t.Fatalf("cmdLogs on Windows = %v, want the not-supported notice and nil", errLogs)
		}
	} else if errLogs == nil || !strings.Contains(errLogs.Error(), "ran out of budget") {
		t.Fatalf("cmdLogs must refuse unitless provider on cut pass, got: %v", errLogs)
	}

	// cmdSet refuses
	errSet := cmdSet([]string{"node-name", "edge01"}, false, true)
	if errSet == nil || !strings.Contains(errSet.Error(), "ran out of budget") {
		t.Fatalf("cmdSet must refuse unitless provider on cut pass, got: %v", errSet)
	}
}
