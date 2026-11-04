package miner

import "github.com/urnetwork/sdk"

// extender_detail.go carries the extender's descriptive fields for the control
// reply only.
//
// providerExtenderObservation stays a closed set of scalar facts: the listener
// compares it with == to decide whether anything changed, and the diagnostic
// record is built from it. Free text and live counters must not join it, or
// every ping would count as a change and an SDK error string would sit inside a
// type whose contract is that arbitrary text never reaches the output. The
// detail travels beside it and is read only by `urnet-tools extender status`
// and `top`.

// extenderDetailTextMax bounds each free-text field. The SDK error text is not
// bounded, and this reply is read over a local socket by a terminal.
const extenderDetailTextMax = 512

type providerExtenderDetail struct {
	Ipv4                  string `json:"ipv4,omitempty"`
	Ipv6                  string `json:"ipv6,omitempty"`
	LastActivationError   string `json:"last_activation_error,omitempty"`
	DnsPorts              string `json:"dns_ports,omitempty"`
	ConnectionCount       int    `json:"connection_count,omitempty"`
	PeerPingCount         int    `json:"peer_ping_count,omitempty"`
	PeerPingCosignedCount int    `json:"peer_ping_cosigned_count,omitempty"`
	PeerPingRejectedCount int    `json:"peer_ping_rejected_count,omitempty"`
	PeerPingUnknownCount  int    `json:"peer_ping_unknown_count,omitempty"`
	LastPeerPingTime      int64  `json:"last_peer_ping_time,omitempty"`
	LimitedBySubnetsCount int    `json:"limited_by_subnets_count,omitempty"`
	LimitedBySourceCount  int    `json:"limited_by_source_count,omitempty"`
}

// providerExtenderControlReading is the control reply's extender object: the
// observation's keys and the detail's keys in one flat JSON object.
type providerExtenderControlReading struct {
	providerExtenderObservation
	providerExtenderDetail
}

func extenderDetailText(s string) string {
	if len(s) <= extenderDetailTextMax {
		return s
	}
	return s[:extenderDetailTextMax]
}

func extenderDetailFromStatus(status *sdk.ExtenderProvideStatus) providerExtenderDetail {
	if status == nil {
		return providerExtenderDetail{}
	}
	return providerExtenderDetail{
		Ipv4:                  extenderDetailText(status.Ipv4),
		Ipv6:                  extenderDetailText(status.Ipv6),
		LastActivationError:   extenderDetailText(status.LastActivationError),
		DnsPorts:              extenderDetailText(status.DnsPorts),
		ConnectionCount:       status.ConnectionCount,
		PeerPingCount:         status.PeerPingCount,
		PeerPingCosignedCount: status.PeerPingCosignedCount,
		PeerPingRejectedCount: status.PeerPingRejectedCount,
		PeerPingUnknownCount:  status.PeerPingUnknownCount,
		LastPeerPingTime:      status.LastPeerPingTime,
		LimitedBySubnetsCount: status.LimitedBySubnetsCount,
		LimitedBySourceCount:  status.LimitedBySourceCount,
	}
}
