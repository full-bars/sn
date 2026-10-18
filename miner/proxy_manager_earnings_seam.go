package miner

// The v33 per-proxy earn tracker (provider/earn_tracker.go) is G-06. One
// P1(c) reader needs only its constants until the tracker lands:
//
//   - the paid grader: paidForceProbeCeiling (the hard probe ceiling) and,
//     through the proxyEarnedRecentlyFor seam below, paidEarnWindow.
//
// The names are the v33 ones so the tracker port can adopt this file's
// contents and delete the seam.

import (
	"time"
)

const (
	// paidEarnWindow is the rolling window the per-proxy earn tracker credits
	// billable traffic over (v33 earn_tracker.go). The audit uses it to decide
	// when "no recent earnings" means something after a restart.
	paidEarnWindow = 15 * time.Minute

	// paidForceProbeCeiling is the hard upper bound on how long a paid proxy
	// can go without a real stage-1 probe regardless of earn state (v33
	// earn_tracker.go). The paid grader reads it; the tracker port adopts this
	// constant with the rest of that file.
	paidForceProbeCeiling = 24 * time.Hour
)

// proxyEarnedRecentlyFor reports whether addr earned within paidEarnWindow
// (v33 globalPerProxyEarnTracker.EarnedSince). The in-memory tracker is
// G-06, so the seam says "no recent earnings" until it lands; the audit's
// bandwidth-unknown veto is what keeps a park from acting on that absence.
var proxyEarnedRecentlyFor = func(addr string) bool { return false }
