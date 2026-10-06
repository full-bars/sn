package provider

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/connect"
)

// QUIC DATAGRAM on sn's own H3 connections.
//
// sn builds its platform transport settings from
// connect.DefaultPlatformTransportSettings, which sets EnableH3Datagrams to
// true. Nothing in provider/ ever changed it, so every H3 connection sn opened
// offered DATAGRAM, with no gate, no health line and no metric. This file is
// the operator surface that was missing: a process-wide runtime gate for the
// offer and for the send lane, a registry so a live change reaches the running
// transport, and the counters the health line and /metrics read.
//
// The pinned connect revision exposes the DATAGRAM data plane but not the
// fork's synchronized gate: connect.SetH3DatagramsEnabled lives in the fork's
// own transport_h3_gate.go, which is not published on full-bars/connect, so sn
// carries the gate itself and drives the two seams the pinned connect does
// expose:
//
//   - the offer is read from PlatformTransportSettings.EnableH3Datagrams when
//     an H3 connection is dialed, so a change is written to the live settings
//     and the transport is kicked to re-dial;
//   - the send lane is chosen per message from
//     H3DatagramSettings.HybridDatagramMessageByteCount, so a change takes
//     effect on the next message with no reconnect.
//
// Both gates default off. That is a deliberate change from connect's own
// default-true: an operator opts in, and a box that never touches the key no
// longer offers DATAGRAM silently.

var (
	// h3DatagramOffer is the runtime switch for OFFERING QUIC DATAGRAM on an H3
	// connection. Off unless an operator turns it on.
	h3DatagramOffer atomic.Bool
	// h3DatagramSend is the runtime switch for SENDING routed frames as
	// DATAGRAM on a connection where the server accepted the offer. Off means
	// the receive side still works and everything sn sends stays on the stream.
	h3DatagramSend atomic.Bool
)

// h3DatagramDefaultSendThreshold is the largest complete routed frame connect
// will put on DATAGRAM, captured from connect's own default so the on-value
// never drifts from the engine. The send gate sets the live threshold to this
// when it is on and to zero when it is off.
var h3DatagramDefaultSendThreshold = func() int {
	s := connect.DefaultH3DatagramSettings()
	if s == nil {
		return 0
	}
	return s.HybridDatagramMessageByteCount
}()

// h3DatagramTarget is one running platform transport's settings and transport,
// kept so a gate change can reach the live object.
type h3DatagramTarget struct {
	settings  *connect.PlatformTransportSettings
	transport *connect.PlatformTransport
}

var (
	h3DatagramTargetsMu sync.Mutex
	h3DatagramTargets   []*h3DatagramTarget
)

// h3DatagramProcessStats is one collector shared by every platform transport
// sn builds. connect aggregates connection generations behind an injected
// *H3DatagramStats, so a single process-level collector means the health line
// and /metrics report the whole process rather than whichever transport happens
// to be last in the registry, and the counters never move backwards when a
// transport disconnects or a proxy reloads.
var h3DatagramProcessStats = &connect.H3DatagramStats{}

// h3DatagramOfferValue is the offer value to write onto a settings object.
func h3DatagramOfferValue() bool {
	return h3DatagramOffer.Load()
}

// applyH3DatagramOfferToSettings writes the offer gate onto one identity's
// settings object. This is the exact wiring provideWithProxy runs before it
// constructs the platform transport: connect reads the field at each H3 dial.
func applyH3DatagramOfferToSettings(settings *connect.PlatformTransportSettings) {
	if settings == nil {
		return
	}
	settings.EnableH3Datagrams = h3DatagramOfferValue()
}

// applyH3DatagramSendToSettings writes the send-lane threshold onto one
// identity's settings object. provideWithProxy runs this AFTER the platform
// transport is constructed, because the constructor's Validate rejects a zero
// threshold while a zero set afterwards is what keeps sends on the stream.
func applyH3DatagramSendToSettings(settings *connect.PlatformTransportSettings) {
	if settings == nil || settings.H3DatagramSettings == nil {
		return
	}
	settings.H3DatagramSettings.HybridDatagramMessageByteCount = h3DatagramSendThresholdValue()
}

// h3DatagramSendThresholdValue is the send-lane threshold to write onto a
// settings object: connect's default when the send gate is on, zero when it is
// off. A zero threshold makes UseDatagramForPath reject every frame, so all
// sends stay on the reliable stream.
func h3DatagramSendThresholdValue() int {
	if h3DatagramSend.Load() {
		return h3DatagramDefaultSendThreshold
	}
	return 0
}

// H3DatagramOfferEnabled reports whether the offer gate is on.
func H3DatagramOfferEnabled() bool {
	return h3DatagramOffer.Load()
}

// H3DatagramSendEnabled reports whether the send gate is on.
func H3DatagramSendEnabled() bool {
	return h3DatagramSend.Load()
}

// SetH3DatagramOffer turns the DATAGRAM offer on or off at runtime and returns
// the previous value. The offer is read when an H3 connection is dialed, so the
// change is written to every registered settings object and each transport is
// kicked: it closes its live connection and re-dials with the new setting.
func SetH3DatagramOffer(enabled bool) (previous bool) {
	previous = h3DatagramOffer.Swap(enabled)
	applyH3DatagramOfferLive()
	return previous
}

// SetH3DatagramSend turns the DATAGRAM send lane on or off at runtime and
// returns the previous value. The lane is chosen per message, so this takes
// effect on the next message with no reconnect.
func SetH3DatagramSend(enabled bool) (previous bool) {
	previous = h3DatagramSend.Swap(enabled)
	applyH3DatagramSendLive()
	return previous
}

// registerH3DatagramTarget records a running platform transport so a gate
// change can reach it. Called by provideWithProxy for an eligible identity.
func registerH3DatagramTarget(settings *connect.PlatformTransportSettings, transport *connect.PlatformTransport) {
	if settings == nil || transport == nil {
		return
	}
	h3DatagramTargetsMu.Lock()
	h3DatagramTargets = append(h3DatagramTargets, &h3DatagramTarget{settings: settings, transport: transport})
	h3DatagramTargetsMu.Unlock()
}

// unregisterH3DatagramTarget drops a transport that has closed. Called on the
// way out of provideWithProxy.
func unregisterH3DatagramTarget(transport *connect.PlatformTransport) {
	if transport == nil {
		return
	}
	h3DatagramTargetsMu.Lock()
	filtered := h3DatagramTargets[:0]
	for _, t := range h3DatagramTargets {
		if t.transport != transport {
			filtered = append(filtered, t)
		}
	}
	// Clear the truncated tail: the backing array still references the closed
	// transport and its settings past the new length, which would keep them
	// reachable until the slice reallocates.
	for i := len(filtered); i < len(h3DatagramTargets); i++ {
		h3DatagramTargets[i] = nil
	}
	h3DatagramTargets = filtered
	h3DatagramTargetsMu.Unlock()
}

// applyH3DatagramOfferLive writes the offer gate to every registered settings
// object and kicks each transport so the next dial reads it.
func applyH3DatagramOfferLive() {
	offer := h3DatagramOfferValue()
	h3DatagramTargetsMu.Lock()
	targets := append([]*h3DatagramTarget(nil), h3DatagramTargets...)
	h3DatagramTargetsMu.Unlock()
	for _, t := range targets {
		t.settings.EnableH3Datagrams = offer
		t.transport.Kick()
	}
}

// applyH3DatagramSendLive writes the send-lane threshold to every registered
// settings object. No kick: the threshold is read per message.
//
// The pinned connect reads H3DatagramSettings.HybridDatagramMessageByteCount
// from its per-connection send workers without a lock (transport.go
// UseDatagramForPath), so a live change is a data race under the Go memory
// model: connect exposes no synchronized setter for this field on this
// revision. The write is skipped when the value already matches, which keeps
// the steady state free of writes, but a real transition still writes the field
// once. Removing the race altogether needs a connect-side atomic setter, which
// is out of sn's hands.
func applyH3DatagramSendLive() {
	threshold := h3DatagramSendThresholdValue()
	h3DatagramTargetsMu.Lock()
	targets := append([]*h3DatagramTarget(nil), h3DatagramTargets...)
	h3DatagramTargetsMu.Unlock()
	for _, t := range targets {
		if t.settings.H3DatagramSettings == nil {
			continue
		}
		if t.settings.H3DatagramSettings.HybridDatagramMessageByteCount == threshold {
			continue
		}
		t.settings.H3DatagramSettings.HybridDatagramMessageByteCount = threshold
	}
}

// h3DatagramStatsSnapshot returns the process-wide DATAGRAM counters, or
// ok=false when no eligible transport is registered. The health line and
// /metrics both read this.
//
// Every transport writes into the one h3DatagramProcessStats collector, so this
// is the sum over every running identity and every reconnect generation, not a
// sample of one transport: a churning proxy set cannot make the counters drop
// or reset.
func h3DatagramStatsSnapshot() (snapshot connect.H3DatagramStatsSnapshot, ok bool) {
	h3DatagramTargetsMu.Lock()
	registered := 0 < len(h3DatagramTargets)
	h3DatagramTargetsMu.Unlock()
	if !registered {
		return snapshot, false
	}
	return h3DatagramProcessStats.Snapshot(), true
}

// h3DatagramMetricsEnabled reports whether /metrics should serve the
// urnet_h3_datagram_* families for this snapshot: once an operator turns the
// gate on, or once a connection has carried a datagram, so a box that never
// enables the feature serves none of them. Without this an H3 set that is on by
// default would export ten all-zero families while the DATAGRAM gate is off.
func h3DatagramMetricsEnabled(snapshot connect.H3DatagramStatsSnapshot) bool {
	return H3DatagramOfferEnabled() ||
		H3DatagramSendEnabled() ||
		0 < snapshot.ReceivedMessageCount ||
		0 < snapshot.SentMessageCount
}

// h3DatagramHealthSuffix renders the DATAGRAM part of the [health] line, or ""
// until an H3 connection has received a datagram, so a box that never turns the
// feature on grows no fields it cannot fill.
//
// The pinned connect exposes no offered/accepted or blackhole counters (the
// fork added those inside connect), so the suffix reports only what the engine
// can count here: received messages and reassembly drops, and, once anything was
// sent as a datagram, the send-lane split and send errors.
func h3DatagramHealthSuffix(snapshot connect.H3DatagramStatsSnapshot) string {
	if snapshot.ReceivedMessageCount == 0 && snapshot.SentMessageCount == 0 {
		return ""
	}
	out := fmt.Sprintf(" h3_dg_rx=%d h3_dg_rx_drop=%d",
		snapshot.ReceivedMessageCount, snapshot.ReassemblyLimitCount)
	if snapshot.SentMessageCount != 0 || snapshot.SendErrorCount != 0 {
		out += fmt.Sprintf(" dg_tx=%d dg_tx_stream=%d dg_tx_err=%d",
			snapshot.SentMessageCount, snapshot.StreamSentMessageCount, snapshot.SendErrorCount)
	}
	return out
}

// writeH3DatagramMetrics renders the urnet_h3_datagram_* families from the
// running transport's counters. Families the pinned connect cannot count
// (offered, accepted, blackholes) are intentionally absent rather than exported
// as a constant zero, so a missing series means unknown.
func writeH3DatagramMetrics(b *strings.Builder, snapshot connect.H3DatagramStatsSnapshot) {
	fmt.Fprintf(b, "# HELP urnet_h3_datagram_rx_messages_total Messages received over DATAGRAM.\n")
	fmt.Fprintf(b, "# TYPE urnet_h3_datagram_rx_messages_total counter\n")
	fmt.Fprintf(b, "urnet_h3_datagram_rx_messages_total %d\n", snapshot.ReceivedMessageCount)
	fmt.Fprintf(b, "# HELP urnet_h3_datagram_rx_bytes_total Bytes of messages received over DATAGRAM.\n")
	fmt.Fprintf(b, "# TYPE urnet_h3_datagram_rx_bytes_total counter\n")
	fmt.Fprintf(b, "urnet_h3_datagram_rx_bytes_total %d\n", snapshot.ReceivedMessageByteCount)
	fmt.Fprintf(b, "# HELP urnet_h3_datagram_rx_dropped_total Received DATAGRAM messages dropped by the reassembly limit.\n")
	fmt.Fprintf(b, "# TYPE urnet_h3_datagram_rx_dropped_total counter\n")
	fmt.Fprintf(b, "urnet_h3_datagram_rx_dropped_total %d\n", snapshot.ReassemblyLimitCount)
	fmt.Fprintf(b, "# HELP urnet_h3_datagram_rx_timeouts_total Received DATAGRAM messages dropped by the reassembly timeout.\n")
	fmt.Fprintf(b, "# TYPE urnet_h3_datagram_rx_timeouts_total counter\n")
	fmt.Fprintf(b, "urnet_h3_datagram_rx_timeouts_total %d\n", snapshot.ReassemblyTimeoutCount)
	fmt.Fprintf(b, "# HELP urnet_h3_datagram_rx_rejected_total DATAGRAMs refused by the datagram layer.\n")
	fmt.Fprintf(b, "# TYPE urnet_h3_datagram_rx_rejected_total counter\n")
	fmt.Fprintf(b, "urnet_h3_datagram_rx_rejected_total{reason=\"malformed\"} %d\n", snapshot.MalformedFragmentCount)
	fmt.Fprintf(b, "urnet_h3_datagram_rx_rejected_total{reason=\"duplicate\"} %d\n", snapshot.DuplicateFragmentCount)
	fmt.Fprintf(b, "urnet_h3_datagram_rx_rejected_total{reason=\"checksum\"} %d\n", snapshot.ChecksumFailureCount)
	fmt.Fprintf(b, "# HELP urnet_h3_datagram_tx_messages_total Messages sent over DATAGRAM.\n")
	fmt.Fprintf(b, "# TYPE urnet_h3_datagram_tx_messages_total counter\n")
	fmt.Fprintf(b, "urnet_h3_datagram_tx_messages_total %d\n", snapshot.SentMessageCount)
	fmt.Fprintf(b, "# HELP urnet_h3_datagram_tx_bytes_total Bytes of messages sent over DATAGRAM.\n")
	fmt.Fprintf(b, "# TYPE urnet_h3_datagram_tx_bytes_total counter\n")
	fmt.Fprintf(b, "urnet_h3_datagram_tx_bytes_total %d\n", snapshot.SentMessageByteCount)
	fmt.Fprintf(b, "# HELP urnet_h3_datagram_tx_stream_messages_total Messages sent on the reliable stream of a connection that negotiated DATAGRAM, so the lane split is tx_messages against this.\n")
	fmt.Fprintf(b, "# TYPE urnet_h3_datagram_tx_stream_messages_total counter\n")
	fmt.Fprintf(b, "urnet_h3_datagram_tx_stream_messages_total %d\n", snapshot.StreamSentMessageCount)
	fmt.Fprintf(b, "# HELP urnet_h3_datagram_tx_errors_total DATAGRAM send errors.\n")
	fmt.Fprintf(b, "# TYPE urnet_h3_datagram_tx_errors_total counter\n")
	fmt.Fprintf(b, "urnet_h3_datagram_tx_errors_total %d\n", snapshot.SendErrorCount)
}
