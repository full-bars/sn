package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// Adaptation notes:
// - Ported from main to package provider.
// - connect.SetSharedDohCache does not exist in v2026 connect; replaced with
//   package-local SetSharedDohCache and SharedDohCache.
// - cache.Warm in v2026 connect takes (ctx context.Context, serverCount int) and
//   is blocking; launched in a background goroutine with count=4 matching
//   fork behavior.
// - Replaced unexported tlog with dohLog to prevent package namespace collisions.

const dohScoresFilename = ".doh_scores"
const dohScoresSaveInterval = 5 * time.Minute

var dohScoresMu sync.Mutex

var (
	sharedDohCacheMu  sync.Mutex
	sharedDohCacheVal *connect.DohCache
)

// SetSharedDohCache registers a persistent DohCache for proxy target
// resolution.
func SetSharedDohCache(c *connect.DohCache) {
	sharedDohCacheMu.Lock()
	sharedDohCacheVal = c
	sharedDohCacheMu.Unlock()
}

// SharedDohCache returns the registered shared DohCache, or nil if none.
func SharedDohCache() *connect.DohCache {
	sharedDohCacheMu.Lock()
	defer sharedDohCacheMu.Unlock()
	return sharedDohCacheVal
}

// dohFailureCount tracks DNS-over-HTTPS query failures observed at the
// provider level. The fork's connect.GetDohFailureCount was removed in
// v2026 connect; this counter fills the gap so getDohFailureCountStub()
// returns live data rather than a hardcoded zero.
//
// Callers that observe DOH query failures should call IncrDohFailure()
// to increment. Currently no call sites exist because DOH queries are
// performed inside the connect module; this counter is wired for future
// instrumentation or when a provider-level DOH wrapper is added.
var dohFailureCount atomic.Int64

// IncrDohFailure atomically increments the DOH failure counter.
func IncrDohFailure() {
	dohFailureCount.Add(1)
}

// dohFailureCountValue returns the current DOH failure count.
func dohFailureCountValue() int64 {
	return dohFailureCount.Load()
}

func dohLog(format string, args ...any) {
	fmt.Printf("%s "+format, append([]any{time.Now().Format("0102 15:04:05")}, args...)...)
}

// dohScoresPath returns the path to the persisted server-score file.
func dohScoresPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", dohScoresFilename), nil
}

// loadDohScores reads the previously-persisted server scores from disk.
// Returns an empty map when the file doesn't exist (first run).
func loadDohScores() (map[string]float64, error) {
	p, err := dohScoresPath()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]float64{}, nil
		}
		return nil, err
	}
	var scores map[string]float64
	if err := json.Unmarshal(b, &scores); err != nil {
		return nil, err
	}
	if scores == nil {
		scores = map[string]float64{}
	}
	return scores, nil
}

// saveDohScores writes the current server scores to disk as JSON,
// atomically (tmp+rename), with 0600 permissions. Creates the
// ~/.urnetwork directory if it does not exist.
func saveDohScores(scores map[string]float64) error {
	if len(scores) == 0 {
		return nil
	}
	dohScoresMu.Lock()
	defer dohScoresMu.Unlock()
	p, err := dohScoresPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	b, err := json.Marshal(scores)
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// initPersistentDohCache creates a DohCache with persisted server scores,
// starts the warm-up probe, and spawns periodic score persistence.
// Returns the cache and a save+close function for the caller to defer.
func initPersistentDohCache(ctx context.Context) (*connect.DohCache, func()) {
	settings := connect.DefaultDohSettings()
	settings.DnsResolverSettings = connect.DefaultDnsResolverSettings()

	// Load last session's scores so the fan-out starts already biased
	// toward the servers that were fastest last session.
	if scores, err := loadDohScores(); err != nil {
		dohLog("[doh] warning: could not load persisted server scores: %v\n", err)
	} else if len(scores) > 0 {
		settings.ServerStatsSeed = scores
	}

	cache := connect.NewDohCache(settings)
	SetSharedDohCache(cache)
	go cache.Warm(ctx, 4)

	// Periodic persistence goroutine — saves scores every 5m so a crash
	// loses at most one window of signal.
	go func() {
		ticker := time.NewTicker(dohScoresSaveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if scores := cache.ServerScores(); len(scores) > 0 {
					if err := saveDohScores(scores); err != nil {
						dohLog("[doh] warning: could not persist server scores: %v\n", err)
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Returns a deferred cleanup that saves final scores, clears the shared
	// cache reference, and shuts down in-flight queries.
	close := func() {
		if scores := cache.ServerScores(); len(scores) > 0 {
			if err := saveDohScores(scores); err != nil {
				dohLog("[doh] warning: could not persist server scores on shutdown: %v\n", err)
			}
		}
		SetSharedDohCache(nil)
		cache.Close()
	}

	return cache, close
}
