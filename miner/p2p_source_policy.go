package miner

import (
	"fmt"
	"strings"

	connect "github.com/urnetwork/connect"
)

// replayP2PSourcePolicyAtStartup applies the persisted p2p source controls
// before any provider builds a transport. The two values live in the pinned
// connect's process state, which a restart resets to the library defaults, so
// the miner replays them here the way it replays h3 and smart_dialer. A key
// with no persisted value takes the default from controlLiveDefaults.
func replayP2PSourcePolicyAtStartup(state *controlState) {
	for _, key := range []string{"p2p_source_binding", "p2p_forged_source_drop"} {
		value := controlLiveDefaults[key]
		if v, ok := state.get(key); ok && strings.TrimSpace(v) != "" {
			value = v
		}
		if err := applyControlLiveSideEffect(key, value); err != nil {
			tlog("[control] failed to apply %s=%s at startup: %s\n", key, value, err)
		}
	}
}

// writeP2PSourceMetrics renders the two source policy families from the pinned
// connect's process counters. connect exposes no metrics endpoint of its own,
// so the miner writes them here: one family for the enforced drops, split by
// the name the frame claimed, and one for the frames a bound lane carried
// under another source, split by the mode in force and by what happened to the
// frame. Every value is a fixed label set, so a scrape never creates a new
// series from traffic, and the zero before the first event keeps panels and
// alerts on stable series. Both are in-process counters and are not persisted
// across restarts.
func writeP2PSourceMetrics(b *strings.Builder) {
	stats := connect.P2PSourceStatsSnapshot()
	fmt.Fprintf(b, "# HELP urnet_p2p_forged_source_drops_total Inbound frames dropped on a direct peer-to-peer lane that named the platform or this client.\n")
	fmt.Fprintf(b, "# TYPE urnet_p2p_forged_source_drops_total counter\n")
	fmt.Fprintf(b, "urnet_p2p_forged_source_drops_total{kind=\"platform\"} %d\n", stats.ForgedPlatformDropCount)
	fmt.Fprintf(b, "urnet_p2p_forged_source_drops_total{kind=\"self\"} %d\n", stats.ForgedSelfDropCount)
	fmt.Fprintf(b, "# HELP urnet_p2p_source_mismatch_total Inbound frames on a lane bound to one peer that named a different source, by policy mode and what happened to the frame.\n")
	fmt.Fprintf(b, "# TYPE urnet_p2p_source_mismatch_total counter\n")
	fmt.Fprintf(b, "urnet_p2p_source_mismatch_total{mode=\"count\",outcome=\"decoded\"} %d\n", stats.MismatchCountDecoded)
	fmt.Fprintf(b, "urnet_p2p_source_mismatch_total{mode=\"count\",outcome=\"rejected\"} %d\n", stats.MismatchCountRejected)
	fmt.Fprintf(b, "urnet_p2p_source_mismatch_total{mode=\"count\",outcome=\"dropped\"} %d\n", stats.MismatchCountDropped)
	fmt.Fprintf(b, "urnet_p2p_source_mismatch_total{mode=\"drop\",outcome=\"dropped\"} %d\n", stats.MismatchDropDropped)
}
