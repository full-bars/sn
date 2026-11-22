package miner

import (
	"sort"
	"time"
)

// measurementCandidate holds scheduling evidence separately from the platform
// TLS verdict. Its timestamps retain monotonic readings for due decisions.
type measurementCandidate struct {
	key         string
	udpAt       time.Time
	udpInterval time.Duration
	ipv6Due     time.Time
	attemptedAt time.Time
	dueAt       time.Time
}

// selectMeasurementSweep orders due identities and caps one dispatch batch.
// Cancelled attempts sort after unattempted identities and then by attempt age.
func selectMeasurementSweep(now time.Time, candidates []measurementCandidate, limit int) []measurementCandidate {
	if limit <= 0 {
		return nil
	}
	due := make([]measurementCandidate, 0, len(candidates))
	for _, c := range candidates {
		udpDue := c.udpAt.IsZero() || (c.udpInterval > 0 && !now.Before(c.udpAt.Add(c.udpInterval)))
		ipv6Due := !c.ipv6Due.IsZero() && !now.Before(c.ipv6Due)
		if !udpDue && !ipv6Due {
			continue
		}
		if c.udpAt.IsZero() {
			c.dueAt = time.Time{}
		} else if udpDue {
			c.dueAt = c.udpAt.Add(c.udpInterval).Round(0)
		}
		if ipv6Due && !c.udpAt.IsZero() && (!udpDue || c.ipv6Due.Round(0).Before(c.dueAt)) {
			c.dueAt = c.ipv6Due.Round(0)
		}
		due = append(due, c)
	}
	sort.Slice(due, func(i, j int) bool {
		a, b := due[i], due[j]
		aa, ba := !a.attemptedAt.IsZero(), !b.attemptedAt.IsZero()
		if aa != ba {
			return !aa
		}
		at, bt := a.attemptedAt.Round(0), b.attemptedAt.Round(0)
		if !at.Equal(bt) {
			return at.Before(bt)
		}
		if !a.dueAt.Equal(b.dueAt) {
			return a.dueAt.Before(b.dueAt)
		}
		return a.key < b.key
	})
	if len(due) > limit {
		due = due[:limit]
	}
	return due
}
