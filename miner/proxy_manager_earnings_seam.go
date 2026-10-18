package miner

// The v33 earnings store (provider/proxy_earnings_store.go) and the per-proxy
// earn tracker (provider/earn_tracker.go) are P1(c2). Two P1(c) readers need
// only their constants and the decayed-earnings lookup, so the port keeps
// those here until the store lands:
//
//   - proxy audit: earningsMinRetainedScore (the park veto), paidEarnWindow
//     (the tracker warm-up window);
//   - the recovery scout: earningsPromotionBytes and the score lookup.
//
// The names are the v33 ones so the store port can adopt this file's contents
// and delete the seam.

import (
	"time"
)

const (
	// earningsMinRetainedScore drops sub-byte decayed residue on save, so a
	// store never persists an entry that is effectively zero. The audit's
	// park veto reads it: a proxy whose retained earnings score is at least
	// this is treated as an earner.
	earningsMinRetainedScore = 1.0

	// paidEarnWindow is the rolling window the per-proxy earn tracker credits
	// billable traffic over (v33 earn_tracker.go). The audit uses it to decide
	// when "no recent earnings" means something after a restart.
	paidEarnWindow = 15 * time.Minute

	// paidForceProbeCeiling is the hard upper bound on how long a paid proxy
	// can go without a real stage-1 probe regardless of earn state (v33
	// earn_tracker.go). The paid grader reads it; the tracker port adopts this
	// constant with the rest of that file.
	paidForceProbeCeiling = 24 * time.Hour

	// earningsPromotionBytes is the decayed score at which a URL-sourced proxy
	// stops being treated as an unproven address and is ordered alongside the
	// file list. The scout's file bonus equals it on purpose.
	earningsPromotionBytes = 64 << 20 // 64 MiB
)

// proxyEarningsScoreFor is the decayed earnings lookup the scout ranks with
// (v33 proxyEarningsScore). The earnings store is P1(c2), so the lookup is a
// seam that scores zero until it lands; the store port replaces this variable
// with the real reader. Nothing fabricates history without it.
var proxyEarningsScoreFor = func(addr string, now time.Time) float64 { return 0 }

// proxyEarnedRecentlyFor reports whether addr earned within paidEarnWindow
// (v33 globalPerProxyEarnTracker.EarnedSince). The in-memory tracker is
// P1(c2), so the seam says "no recent earnings" until it lands; the audit's
// bandwidth-unknown veto is what keeps a park from acting on that absence.
var proxyEarnedRecentlyFor = func(addr string) bool { return false }
