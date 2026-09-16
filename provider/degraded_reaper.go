package provider

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

func classifyAuthFailureCause(err error) string {
	errMsg := err.Error()
	switch {
	case strings.Contains(errMsg, "proxy unreachable"):
		return "proxy itself is unreachable (dead/offline SOCKS endpoint — not an API issue)"
	case errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled),
		strings.Contains(errMsg, "Timeout"),
		strings.Contains(errMsg, "timeout"),
		strings.Contains(errMsg, "deadline exceeded"),
		strings.Contains(errMsg, "connection refused"),
		strings.Contains(errMsg, "no such host"):
		return "network error reaching API (check connectivity to api.bringyour.com)"
	default:
		return "API rejected token (check JWT validity)"
	}
}

const (
	degradedReaperTicker      = 3 * time.Minute
	degradedReaperMinDownTime = 30 * time.Minute
	degradedReaperKeepPct     = 50
)

// degradedReaperKeepCount returns how many proxies to keep (ceil rounding).
func degradedReaperKeepCount(total int) int {
	if total <= 0 {
		return 1
	}
	keep := (total*degradedReaperKeepPct + 99) / 100
	if keep < 1 {
		return 1
	}
	return keep
}

// scoredDegradedProxy pairs a degraded proxy with its lifetime-contribution
// score.
type scoredDegradedProxy struct {
	entry DegradedProxyEntry
	score uint64
}

// contractsAcquiredFunc returns the lifetime contracts-acquired count for a
// proxy index, or 0 if unknown.
type contractsAcquiredFunc func(index int) int64

// liveContractsAcquired adapts the real globalContractMetrics registry to
// contractsAcquiredFunc.
func liveContractsAcquired(index int) int64 {
	m := globalContractMetrics.get(index)
	if m == nil {
		return 0
	}
	acquired, _ := m.snapshot()
	return acquired
}

// scoreDegradedProxies ranks degraded proxies by lifetime contribution,
// ascending — the worst contributors sort first.
func scoreDegradedProxies(entries []DegradedProxyEntry, getContracts contractsAcquiredFunc) []scoredDegradedProxy {
	scored := make([]scoredDegradedProxy, len(entries))
	for i, d := range entries {
		score := d.TotalRxBytes + d.TotalTxBytes
		if getContracts != nil {
			if acquired := getContracts(d.Index); acquired > 0 {
				score += uint64(acquired) * 1024
			}
		}
		scored[i] = scoredDegradedProxy{entry: d, score: score}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].score < scored[j].score
	})
	return scored
}

// selectProxiesToReap picks which degraded proxies to cancel.
func selectProxiesToReap(scored []scoredDegradedProxy, keep int, minDownTime time.Duration) []DegradedProxyEntry {
	var toReap []DegradedProxyEntry
	for i := 0; i < len(scored)-keep; i++ {
		p := scored[i].entry
		if p.DownFor < minDownTime {
			continue
		}
		toReap = append(toReap, p)
	}
	return toReap
}

// onlyCancellableProxies filters degraded proxies down to those the reaper
// can actually act on — i.e. present in proxyCancelMap.
func onlyCancellableProxies(degraded []DegradedProxyEntry, proxyCancelMap map[string]context.CancelFunc, proxyCancelMu *sync.Mutex) []DegradedProxyEntry {
	proxyCancelMu.Lock()
	defer proxyCancelMu.Unlock()
	var out []DegradedProxyEntry
	for _, d := range degraded {
		if _, ok := proxyCancelMap[d.Address]; ok {
			out = append(out, d)
		}
	}
	return out
}

// stillDegradedFunc reports whether a proxy address is degraded right now.
type stillDegradedFunc func(address string) bool

// liveIsDegraded adapts the package-local IsDegraded to stillDegradedFunc.
func liveIsDegraded(address string) bool {
	return IsDegraded(address)
}

// reapProxies cancels each candidate in toReap, re-verifying it is still
// degraded before pulling the trigger.
func reapProxies(toReap []DegradedProxyEntry, proxyCancelMap map[string]context.CancelFunc, proxyCancelMu *sync.Mutex, isStillDegraded stillDegradedFunc) int64 {
	var reaped int64
	for _, p := range toReap {
		proxyCancelMu.Lock()
		if !isStillDegraded(p.Address) {
			proxyCancelMu.Unlock()
			continue
		}
		cancel, ok := proxyCancelMap[p.Address]
		if ok {
			cancel()
			delete(proxyCancelMap, p.Address)
			reaped++
		}
		proxyCancelMu.Unlock()
	}
	return reaped
}

func runDegradedProxyReaper(ctx context.Context, proxyCancelMap map[string]context.CancelFunc, proxyCancelMu *sync.Mutex) {
	ticker := time.NewTicker(degradedReaperTicker)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		degraded := DegradedProxies()
		if len(degraded) <= 1 {
			continue
		}

		cancellable := onlyCancellableProxies(degraded, proxyCancelMap, proxyCancelMu)
		if len(cancellable) <= 1 {
			continue
		}

		scored := scoreDegradedProxies(cancellable, liveContractsAcquired)
		keep := degradedReaperKeepCount(len(scored))
		toReap := selectProxiesToReap(scored, keep, degradedReaperMinDownTime)

		reaped := reapProxies(toReap, proxyCancelMap, proxyCancelMu, liveIsDegraded)

		if reaped > 0 {
			tlog("[proxy-quality] dropped %d under-performing proxies (kept best %d of %d)\n",
				reaped, keep, len(scored))
		}
	}
}
