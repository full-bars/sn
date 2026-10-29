package miner

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// slot_select_log.go makes the dialer's select line say which provider slot it
// belongs to, and notices each slot's first successful dial.
//
// connect logs one "🌐 [net][s]select: <dialer> dur=Nms" line per control dial,
// through the logger on the slot's client strategy settings. The line carries
// the transport and its counts but not the slot, so with 500 slots sharing one
// log an operator cannot tell which proxy is talking to the control plane. The
// v3.23 provider names the slot in that line ("proxy[N] (addr) [strategy]");
// this wrapper adds the same label to the rendered line, and reports the
// slot's first successful dial to the bring-up tracker.

const (
	// slotSelectPrefix starts every select line connect logs.
	slotSelectPrefix = "🌐 [net][s]select: "
	// slotSelectSuccessFormat is the exact format of a successful select.
	// Failures use a different format ("... = %s dur=%dms"), so matching the
	// whole string cannot mistake one for the other.
	slotSelectSuccessFormat = slotSelectPrefix + "%s dur=%dms\n"
	// slotFamilyDialPrefix starts connect's per-dial address-family decision.
	// It repeats unchanged on every dial, so the wrapper prints it on change.
	slotFamilyDialPrefix = "[family]dial "
)

// slotSelectLabel names a slot the way the v3.23 provider does.
func slotSelectLabel(index uint64, proxyAddr string) string {
	if proxyAddr == "" {
		return fmt.Sprintf("[direct] provider=%d", index)
	}
	return fmt.Sprintf("proxy[%d] (%s)", index, redactedProxyAddress(proxyAddr))
}

type slotSelectLogger struct {
	connect.Logger
	label string
	// reached is called once, with the first successful dial through this slot.
	reached func(dur time.Duration, via string)
	once    sync.Once

	// callerSink records that the caller supplied the logger. That sink is the
	// caller's own stream, so the lines keep going there; only the library
	// default is replaced by the provider's tab format (see Infof).
	callerSink bool

	// familyLast is the last [family]dial decision this slot printed, so an
	// unchanged decision is not repeated on every dial. A dial that fails still
	// prints, because the failure is the reason to look at the decision.
	mu         sync.Mutex
	familyLast string
}

// withSlotSelectLabel wraps log so the slot's select lines carry its label. A
// nil log resolves to the library default, as the strategy would.
func withSlotSelectLabel(log connect.Logger, label string, reached func(dur time.Duration, via string)) connect.Logger {
	callerSink := log != nil
	if log == nil {
		log = connect.DefaultLogger()
	}
	return &slotSelectLogger{Logger: log, label: label, reached: reached, callerSink: callerSink}
}

func (self *slotSelectLogger) Infof(format string, args ...any) {
	// Render first, then judge the line. connect does not hand its lines to the
	// logger as formats: the family decision arrives as
	// Infof("%s\n", controlDialFamilyLine(...)) (connect/egress_dial.go), so a
	// prefix test on `format` never matched it and every dial printed. The
	// select line is the one line connect passes as a format with its own
	// arguments, and its shape is the only thing the arguments are read for.
	message := fmt.Sprintf(format, args...)
	switch {
	case strings.HasPrefix(message, slotSelectPrefix):
		if format == slotSelectSuccessFormat && self.reached != nil && len(args) == 2 {
			via, _ := args[0].(string)
			ms, _ := args[1].(int64)
			// An extender carrier is a side channel, not the slot's own path
			// to the control plane.
			if !strings.HasPrefix(via, "extender") {
				self.once.Do(func() { self.reached(time.Duration(ms)*time.Millisecond, via) })
			}
		}
		// The label joins the rendered line, so a % in a proxy address or
		// account stays a character instead of becoming a format verb.
		message = slotSelectPrefix + self.label + " " + message[len(slotSelectPrefix):]
	case strings.HasPrefix(message, slotFamilyDialPrefix):
		// A family decision repeats for the life of the slot: the same tag
		// picks the same family on every dial. Print it when it changes, and
		// always when the dial failed, since that is when the decision matters.
		self.mu.Lock()
		repeat := message == self.familyLast && !strings.Contains(message, "err=")
		if !repeat {
			self.familyLast = message
		}
		self.mu.Unlock()
		if repeat {
			return
		}
	}
	// Deliver the line once, where the caller pointed it. A supplied sink is
	// the caller's stream and saw every one of these lines before this wrapper
	// existed, so it still gets them, through the logger, the way the rest of
	// the package forwards. The library default is glog, which would stamp this
	// file and line on a line the dialer produced — long and untrue — so that
	// case, and only that case, goes out in the provider's own tab format.
	// Either way the label is already in the rendered text, so a % in a proxy
	// address or account stays a character.
	if self.callerSink {
		self.Logger.Infof("%s", message)
		return
	}
	tlog("%s", message)
}
