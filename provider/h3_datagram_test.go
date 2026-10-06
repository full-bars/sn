package provider

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The offer gate overwrites connect's own default of true, and the offer
// connect frames follows the gate. Removing the write in
// applyH3DatagramOfferToSettings makes the first assertion fail, because
// connect's default would leave EnableH3Datagrams true.
func TestH3DatagramOfferGateControlsTheOffer(t *testing.T) {
	previousOffer := SetH3DatagramOffer(false)
	previousSend := SetH3DatagramSend(false)
	defer SetH3DatagramOffer(previousOffer)
	defer SetH3DatagramSend(previousSend)

	settings := connect.DefaultPlatformTransportSettings()
	if !settings.EnableH3Datagrams {
		t.Fatal("connect's own default is expected to be true; this test's premise no longer holds")
	}
	applyH3DatagramOfferToSettings(settings)
	if settings.EnableH3Datagrams {
		t.Fatal("offer gate off left EnableH3Datagrams true: DATAGRAM would still be offered with no gate")
	}
	off := &protocol.Auth{}
	connect.SetH3DatagramAuthOffer(off, settings.EnableH3Datagrams)
	if off.H3DatagramVersion != 0 {
		t.Fatalf("offer version = %d, want 0 with the gate off", off.H3DatagramVersion)
	}

	SetH3DatagramOffer(true)
	applyH3DatagramOfferToSettings(settings)
	if !settings.EnableH3Datagrams {
		t.Fatal("offer gate on did not enable EnableH3Datagrams")
	}
	on := &protocol.Auth{}
	connect.SetH3DatagramAuthOffer(on, settings.EnableH3Datagrams)
	if on.H3DatagramVersion != connect.H3DatagramProtocolVersion {
		t.Fatalf("offer version = %d, want %d with the gate on", on.H3DatagramVersion, connect.H3DatagramProtocolVersion)
	}
}

// The send gate drives the per-message threshold the engine reads, and the
// engine's own selector agrees: a zero threshold routes even a tiny frame to
// the stream. Removing the threshold write in applyH3DatagramSendToSettings
// makes the off assertion fail.
func TestH3DatagramSendGateZeroesTheSendThreshold(t *testing.T) {
	previous := SetH3DatagramSend(false)
	defer SetH3DatagramSend(previous)

	settings := connect.DefaultPlatformTransportSettings()
	if settings.H3DatagramSettings == nil {
		t.Fatal("default platform settings carry no H3DatagramSettings; the send gate has nothing to drive")
	}
	applyH3DatagramSendToSettings(settings)
	if settings.H3DatagramSettings.HybridDatagramMessageByteCount != 0 {
		t.Fatalf("send gate off left threshold %d, want 0", settings.H3DatagramSettings.HybridDatagramMessageByteCount)
	}
	// the engine selects the reliable stream for every frame at threshold zero
	off := connect.DefaultH3DatagramSettings()
	off.HybridDatagramMessageByteCount = 0
	if off.UseDatagram(64) {
		t.Fatal("a zero threshold still selected DATAGRAM; sends would not be gated")
	}

	SetH3DatagramSend(true)
	applyH3DatagramSendToSettings(settings)
	if settings.H3DatagramSettings.HybridDatagramMessageByteCount != h3DatagramDefaultSendThreshold {
		t.Fatalf("send gate on set threshold %d, want connect's default %d",
			settings.H3DatagramSettings.HybridDatagramMessageByteCount, h3DatagramDefaultSendThreshold)
	}
	on := connect.DefaultH3DatagramSettings()
	if !on.UseDatagram(1) {
		t.Fatal("connect's default threshold should select DATAGRAM for a small frame")
	}
}

// The rollout predicate is the single hook. Today it covers every identity, so
// narrowing it later is a change here only.
func TestSnH3EligibleCoversEveryIdentityToday(t *testing.T) {
	// The predicate reads the live mode, and other tests leave a cap or `off`
	// in place and restore from a cap name (which re-resolves from disk). Pin
	// `all` here and restore it, or this test fails under a different test
	// order or -shuffle.
	previous, err := SetH3Mode("all")
	if err != nil {
		t.Fatalf("SetH3Mode(all): %v", err)
	}
	defer SetH3Mode(previous)
	if !snH3Eligible(0, nil, true) {
		t.Fatal("direct identity is not eligible; sn runs H3 for it")
	}
	if !snH3Eligible(3, &connect.ProxySettings{}, false) {
		t.Fatal("proxied identity is not eligible; sn runs H3 for it")
	}
}

// Both keys are live: no restart, and the apply path flips the gate. Removing
// the cases from applyLiveSideEffect makes this fail.
func TestH3DatagramControlKeysApplyLive(t *testing.T) {
	if needsRestart("h3_datagram") {
		t.Fatal("h3_datagram must be live, not restart-only")
	}
	if needsRestart("h3_datagram_send") {
		t.Fatal("h3_datagram_send must be live, not restart-only")
	}
	if !controlKeys["h3_datagram"] || !controlKeys["h3_datagram_send"] {
		t.Fatal("controlKeys is missing an h3 datagram key; the socket would reject it")
	}
	previousOffer := SetH3DatagramOffer(false)
	previousSend := SetH3DatagramSend(false)
	defer SetH3DatagramOffer(previousOffer)
	defer SetH3DatagramSend(previousSend)

	if err := applyLiveSideEffect("h3_datagram", "on"); err != nil {
		t.Fatalf("applyLiveSideEffect h3_datagram on: %v", err)
	}
	if !H3DatagramOfferEnabled() {
		t.Fatal("h3_datagram on did not flip the gate")
	}
	if err := applyLiveSideEffect("h3_datagram", "off"); err != nil {
		t.Fatalf("applyLiveSideEffect h3_datagram off: %v", err)
	}
	if H3DatagramOfferEnabled() {
		t.Fatal("h3_datagram off did not flip the gate")
	}
	if err := applyLiveSideEffect("h3_datagram_send", "on"); err != nil {
		t.Fatalf("applyLiveSideEffect h3_datagram_send on: %v", err)
	}
	if !H3DatagramSendEnabled() {
		t.Fatal("h3_datagram_send on did not flip the gate")
	}
}

// The health suffix stays empty until a connection has carried a datagram, and
// names the send lane once anything was sent.
func TestH3DatagramHealthSuffix(t *testing.T) {
	if got := h3DatagramHealthSuffix(connect.H3DatagramStatsSnapshot{}); got != "" {
		t.Fatalf("empty snapshot produced %q, want empty", got)
	}
	snapshot := connect.H3DatagramStatsSnapshot{
		ReceivedMessageCount:   5,
		ReassemblyLimitCount:   2,
		SentMessageCount:       3,
		StreamSentMessageCount: 7,
		SendErrorCount:         1,
	}
	got := h3DatagramHealthSuffix(snapshot)
	for _, want := range []string{"h3_dg_rx=5", "h3_dg_rx_drop=2", "dg_tx=3", "dg_tx_stream=7", "dg_tx_err=1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("health suffix %q missing %q", got, want)
		}
	}
}

// The metric families the fork ships (minus the ones the pinned connect cannot
// count) are rendered.
func TestH3DatagramMetricsFamilies(t *testing.T) {
	var b strings.Builder
	writeH3DatagramMetrics(&b, connect.H3DatagramStatsSnapshot{ReceivedMessageCount: 1})
	out := b.String()
	for _, name := range []string{
		"urnet_h3_datagram_rx_messages_total",
		"urnet_h3_datagram_rx_bytes_total",
		"urnet_h3_datagram_rx_dropped_total",
		"urnet_h3_datagram_rx_timeouts_total",
		"urnet_h3_datagram_rx_rejected_total",
		"urnet_h3_datagram_tx_messages_total",
		"urnet_h3_datagram_tx_bytes_total",
		"urnet_h3_datagram_tx_stream_messages_total",
		"urnet_h3_datagram_tx_errors_total",
	} {
		if !strings.Contains(out, name) {
			t.Fatalf("metrics output missing %s", name)
		}
	}
}

// swapH3DatagramTargets empties the target registry for one test and returns a
// restore func, so a test can control exactly which transports are registered.
func swapH3DatagramTargets() func() {
	h3DatagramTargetsMu.Lock()
	orig := h3DatagramTargets
	h3DatagramTargets = nil
	h3DatagramTargetsMu.Unlock()
	return func() {
		h3DatagramTargetsMu.Lock()
		h3DatagramTargets = orig
		h3DatagramTargetsMu.Unlock()
	}
}

// The snapshot must be the process-wide total, not a sample of one transport.
// With the old "read the last registered transport" behavior a registered
// transport whose own collector is empty reports zero, so the recorded counts
// vanish; here every transport shares one collector and the counts survive.
func TestH3DatagramStatsSnapshotIsProcessWide(t *testing.T) {
	restore := swapH3DatagramTargets()
	defer restore()

	// The counters are package-global and process-wide, so this test must not
	// leak its recorded traffic into other tests reading absolute totals (a
	// -shuffle ordering dependency). Swap in a fresh collector and restore the
	// original on the way out.
	origStats := h3DatagramProcessStats
	h3DatagramProcessStats = &connect.H3DatagramStats{}
	defer func() { h3DatagramProcessStats = origStats }()

	settings := connect.DefaultPlatformTransportSettings()
	registerH3DatagramTarget(settings, &connect.PlatformTransport{})
	registerH3DatagramTarget(settings, &connect.PlatformTransport{})

	before, ok := h3DatagramStatsSnapshot()
	if !ok {
		t.Fatal("snapshot unavailable while a transport is registered")
	}
	h3DatagramProcessStats.RecordStreamSent(17)
	h3DatagramProcessStats.RecordStreamReceived(23)

	after, ok := h3DatagramStatsSnapshot()
	if !ok {
		t.Fatal("snapshot unavailable after recording")
	}
	// RecordStreamSent/Received add one message and the byte count it carries.
	if got := after.StreamSentMessageByteCount - before.StreamSentMessageByteCount; got != 17 {
		t.Fatalf("snapshot stream-sent byte delta = %d, want 17: the counters are not aggregated across transports", got)
	}
	if got := after.StreamReceivedMessageByteCount - before.StreamReceivedMessageByteCount; got != 23 {
		t.Fatalf("snapshot stream-received byte delta = %d, want 23: the counters are not aggregated across transports", got)
	}
	if got := after.StreamSentMessageCount - before.StreamSentMessageCount; got != 1 {
		t.Fatalf("snapshot stream-sent message delta = %d, want 1", got)
	}
}

// A box that never enables the feature must serve no empty datagram families;
// once the gate is on, or a connection has carried a datagram, they are served.
func TestH3DatagramMetricsEnabledGating(t *testing.T) {
	prevOffer := SetH3DatagramOffer(false)
	prevSend := SetH3DatagramSend(false)
	defer SetH3DatagramOffer(prevOffer)
	defer SetH3DatagramSend(prevSend)

	if h3DatagramMetricsEnabled(connect.H3DatagramStatsSnapshot{}) {
		t.Fatal("gate off and no traffic must not serve the families")
	}
	if !h3DatagramMetricsEnabled(connect.H3DatagramStatsSnapshot{ReceivedMessageCount: 1}) {
		t.Fatal("carried traffic must serve the families even with the gate off")
	}
	if !h3DatagramMetricsEnabled(connect.H3DatagramStatsSnapshot{SentMessageCount: 1}) {
		t.Fatal("sent datagrams must serve the families even with the gate off")
	}
	SetH3DatagramOffer(true)
	if !h3DatagramMetricsEnabled(connect.H3DatagramStatsSnapshot{}) {
		t.Fatal("the gate on must serve the families")
	}
}

// Dropping a closed transport must not retain it in the slice's truncated
// backing-array tail. Leaving the tail populated makes this fail.
func TestUnregisterH3DatagramTargetClearsTruncatedSlot(t *testing.T) {
	restore := swapH3DatagramTargets()
	defer restore()

	settings := connect.DefaultPlatformTransportSettings()
	t1 := &connect.PlatformTransport{}
	t2 := &connect.PlatformTransport{}
	registerH3DatagramTarget(settings, t1)
	registerH3DatagramTarget(settings, t2)

	unregisterH3DatagramTarget(t1)

	h3DatagramTargetsMu.Lock()
	live := h3DatagramTargets
	backing := live[:cap(live)]
	h3DatagramTargetsMu.Unlock()

	if len(live) != 1 || live[0].transport != t2 {
		t.Fatalf("registry after unregister = %d entries (first transport %p), want just t2", len(live), t2)
	}
	for i := len(live); i < len(backing); i++ {
		if backing[i] != nil {
			t.Fatalf("backing slot %d still holds %p after truncation; the closed transport is retained", i, backing[i])
		}
	}
}
