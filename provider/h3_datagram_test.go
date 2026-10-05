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
