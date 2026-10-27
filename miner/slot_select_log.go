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
// this wrapper adds the same label by rewriting the format, and reports the
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
	return fmt.Sprintf("proxy[%d] (%s)", index, proxyAddr)
}

type slotSelectLogger struct {
	connect.Logger
	label string
	// reached is called once, with the first successful dial through this slot.
	reached func(dur time.Duration, via string)
	once    sync.Once

	// familyLast is the last [family]dial decision this slot printed, so an
	// unchanged decision is not repeated on every dial. A dial that fails still
	// prints, because the failure is the reason to look at the decision.
	mu         sync.Mutex
	familyLast string
}

// withSlotSelectLabel wraps log so the slot's select lines carry its label. A
// nil log resolves to the library default, as the strategy would.
func withSlotSelectLabel(log connect.Logger, label string, reached func(dur time.Duration, via string)) connect.Logger {
	if log == nil {
		log = connect.DefaultLogger()
	}
	return &slotSelectLogger{Logger: log, label: strings.ReplaceAll(label, "%", "%%"), reached: reached}
}

func (self *slotSelectLogger) Infof(format string, args ...any) {
	if strings.HasPrefix(format, slotSelectPrefix) {
		if format == slotSelectSuccessFormat && self.reached != nil && len(args) == 2 {
			via, _ := args[0].(string)
			ms, _ := args[1].(int64)
			// An extender carrier is a side channel, not the slot's own path
			// to the control plane.
			if !strings.HasPrefix(via, "extender") {
				self.once.Do(func() { self.reached(time.Duration(ms)*time.Millisecond, via) })
			}
		}
		format = slotSelectPrefix + self.label + " " + format[len(slotSelectPrefix):]
	}
	// The wrapper is not the call site. glog would stamp this file and line on
	// every line the slot's strategy logs through it, which is long and untrue,
	// so they all go out in the provider's own tab format instead. Select lines
	// carry the slot label; everything else keeps its own text.
	if strings.HasPrefix(format, slotFamilyDialPrefix) {
		message := fmt.Sprintf(format, args...)
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
		tlog("%s", message)
		return
	}
	tlog(format, args...)
}
