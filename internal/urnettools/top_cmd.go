package urnettools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/urfoundation/sn/internal/tui"
	"github.com/urfoundation/sn/internal/tui/tcellui"
)

// Seams so the command is testable without a terminal or live providers.
var (
	topStdoutIsTerminal = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
	topOpenScreen       = tcellui.Open
	topSourceFn         = func() topSource { return socketTopSource{} }
)

// cmdTop implements `urnet-tools top [target] [--interval D]`, the live
// full-screen view. Target selection is status's: the same flags, the same
// auto-narrowing to the one provider an unprivileged caller can reach, and the
// same cross-user re-exec under sudo.
func cmdTop(args []string) error {
	// --demo is hidden: it draws synthetic snapshots so the screen can be seen
	// on a box with no provider. It is peeled off before the real parsers.
	demo, args := stripDemoFlag(args)
	interval, targetArgs, err := parseTopFlags(args)
	if err != nil {
		return err
	}
	// Checked first: with no terminal there is nothing to draw on, and the
	// question of which provider to show has no bearing on that.
	if !topStdoutIsTerminal() {
		return errors.New("top needs an interactive terminal (stdout is not one); use `urnet-tools status` for one-shot or scripted output, add --json for machine-readable output")
	}
	var t Target
	if !demo {
		t, _, err = parseTargetFlags(targetArgs)
		if err != nil {
			return err
		}
	}
	// The screen opens before discovery, so the operator sees a frame while
	// providers are looked for. Opening it after discovery left the terminal
	// blank for seconds, which reads as a command that does nothing.
	scr, err := topOpenScreen()
	if err != nil {
		return fmt.Errorf("cannot open the terminal for top: %w (use `urnet-tools status` instead)", err)
	}
	defer scr.Close()
	// SIGINT and SIGTERM end the loop through the context, so the terminal is
	// restored by runTop's own exit path rather than by the signal killing the
	// process mid-frame. In raw mode Ctrl-C arrives as a key, not a signal, and
	// is handled by the model.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	providers, cur := []Provider{demoProvider()}, 0
	if !demo {
		var ok bool
		if providers, ok = discoverWhileDrawing(ctx, scr); !ok {
			return nil
		}
		p, _, err := selectTargetOrSoleAccessible(providers, t, false)
		if err != nil {
			// Several providers and no target: status summarizes them, top starts
			// on the first and lets Tab move between them.
			if hasExplicitTarget(t) || len(providers) < 2 {
				holdTopError(ctx, scr, err)
				return err
			}
			p = providers[0]
		}
		// A sudo re-exec cannot share the terminal with a drawn screen, so the
		// screen is given back first. Closing it twice is harmless.
		if crossUserElevationNeeded(p) {
			scr.Close()
		}
		if elevated, err := maybeElevateForCrossUser("top", p, args, false, false); elevated {
			return err
		}
		for i, q := range providers {
			if matchKey(q) == matchKey(p) {
				cur = i
				break
			}
		}
	}
	m := newTopModel(providers, cur, interval, tui.ThemeFromEnv(os.Getenv), time.Now)
	m.refreshExtender()
	if !demo && systemdDiscoveryCutShort {
		m.discoveryNote = discoveryCutShortWarning(len(providers))
	}
	src := topSourceFn()
	if demo {
		src = newDemoTopSource(time.Now)
	} else {
		// Wire the saved menu settings into a real run. The demo deliberately
		// stays out of it — it is a sample, not the user's box.
		applySavedTopSettings(m)
	}
	return runTop(ctx, scr, m, src, topTick)
}

// topDiscoverRedraw is how often the discovery frame's elapsed time is redrawn.
const topDiscoverRedraw = 250 * time.Millisecond

// discoverWhileDrawing runs provider discovery off this goroutine and keeps the
// screen live meanwhile. The frame names what is happening and the seconds
// elapsed, and it is redrawn until discovery answers. Ctrl-C, Esc and q cancel
// the wait, as they do in the main view. ok is false when the operator or a
// signal ended the wait first.
func discoverWhileDrawing(ctx context.Context, scr tcellui.Screen) (providers []Provider, ok bool) {
	systemdDiscoveryCutShort = false
	// The seam is read here, before the goroutine starts, so the goroutine
	// never reads a package variable that someone else may be replacing.
	discover := discoverStatusFn
	done := make(chan []Provider, 1)
	go func() { done <- discover() }()
	start := time.Now()
	ticker := time.NewTicker(topDiscoverRedraw)
	defer ticker.Stop()
	for {
		topNotice(scr,
			"urnet-tools top",
			fmt.Sprintf("looking for providers... %.1fs", time.Since(start).Seconds()),
			"ctrl-c to cancel")
		select {
		case providers = <-done:
			return providers, true
		case <-ticker.C:
		case ev, open := <-scr.Events():
			if !open || quitsTop(ev) {
				return nil, false
			}
		case <-ctx.Done():
			return nil, false
		}
	}
}

// holdTopError keeps a failure to pick a provider on the screen until a key is
// pressed. Otherwise the terminal is restored at once and the reason goes with
// it. The caller still returns err, so the exit code does not change.
func holdTopError(ctx context.Context, scr tcellui.Screen, err error) {
	lines := append(strings.Split(err.Error(), "\n"), "", "press any key to exit")
	for {
		topNotice(scr, lines...)
		select {
		case <-ctx.Done():
			return
		case ev, open := <-scr.Events():
			if !open || ev.Kind != tcellui.EventResize {
				return
			}
		}
	}
}

// topNotice draws plain text lines from the top of the screen, one per row.
func topNotice(scr tcellui.Screen, lines ...string) {
	w, h := scr.Size()
	b := tui.New(w, h)
	for i, line := range lines {
		if i >= h {
			break
		}
		b.Put(line, 0, i, tui.Style{})
	}
	scr.Draw(b)
}

// quitsTop reports whether an input event ends a top screen: Ctrl-C, Esc or q.
func quitsTop(ev tcellui.Event) bool {
	switch ev.Kind {
	case tcellui.EventQuit:
		return true
	case tcellui.EventKey:
		return ev.Key == tcellui.KeyCtrlC || ev.Key == tcellui.KeyEsc
	case tcellui.EventRune:
		return ev.Rune == 'q' || ev.Rune == 'Q'
	}
	return false
}

// applySavedTopSettings points the model at the user's settings file and
// applies what is in it. Without this the menu's save is a silent no-op (no
// path to write to) and a chosen theme or graph style resets on the next run.
func applySavedTopSettings(m *topModel) {
	if m == nil {
		return
	}
	m.settingsPath = topSettingsPath()
	m.applySettings(loadTopSettings(m.settingsPath))
}

// parseTopFlags pulls --interval out of args, leaving the target flags for the
// strict target parser. It accepts a Go duration (500ms, 2s) or plain seconds.
func parseTopFlags(args []string) (interval time.Duration, rest []string, err error) {
	interval = topDefaultInterval
	for i := 0; i < len(args); i++ {
		a := args[i]
		var val string
		switch {
		case a == "--interval":
			if i+1 >= len(args) {
				return 0, nil, errors.New("--interval needs a value, for example --interval 500ms")
			}
			i++
			val = args[i]
		case strings.HasPrefix(a, "--interval="):
			val = strings.TrimPrefix(a, "--interval=")
		default:
			rest = append(rest, a)
			continue
		}
		d, perr := time.ParseDuration(val)
		if perr != nil {
			secs, ferr := strconv.ParseFloat(val, 64)
			if ferr != nil {
				return 0, nil, fmt.Errorf("--interval %q is not a duration (try 500ms or 2s)", val)
			}
			d = time.Duration(secs * float64(time.Second))
		}
		if d < topMinInterval {
			return 0, nil, fmt.Errorf("--interval must be at least %s", topMinInterval)
		}
		interval = d
	}
	return interval, rest, nil
}
