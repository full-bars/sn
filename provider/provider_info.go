package provider

import (
	"time"

	mathrand "math/rand"
)

// proxyURLGiveUpRetryDelay computes the requeue delay for a URL-sourced
// proxy's Nth give-up (giveUpCount is 1-indexed: this call is for the
// Nth give-up). The delay doubles each cycle from proxyURLGiveUpRetryBase
// up to proxyURLGiveUpRetryCap, with up to 20% jitter.
func proxyURLGiveUpRetryDelay(giveUpCount int) time.Duration {
	if giveUpCount < 1 {
		giveUpCount = 1
	}
	delay := proxyURLGiveUpRetryBase
	for i := 1; i < giveUpCount; i++ {
		delay *= 2
		if delay >= proxyURLGiveUpRetryCap {
			delay = proxyURLGiveUpRetryCap
			break
		}
	}
	jitter := time.Duration(mathrand.Int63n(int64(delay)/5 + 1)) // up to 20%
	return delay + jitter
}

// proxyAuthSlowRetryDelay is the backoff for an operator-curated proxy
// (file/internal/direct) that has exhausted its fast retries. Rather than give
// up, it keeps trying on a slow schedule:
//
//   - Attempts 1-3: 5m, 10m, 15m (original ramp — catches brief flapping)
//   - Attempt 4+:    24h (daily — one attempt per day until 14-day drop)
//
// Jitter spreads a large batch so they do not all re-hit the API on the
// same tick.
func proxyAuthSlowRetryDelay(slowAttempt int) time.Duration {
	if slowAttempt < 1 {
		slowAttempt = 1
	}
	var base time.Duration
	if slowAttempt <= slowRetryRampAttempts {
		base = time.Duration(slowAttempt) * 5 * time.Minute
	} else {
		base = slowRetryDailyInterval
	}
	return base + time.Duration(mathrand.Intn(30000))*time.Millisecond
}

// proxyAuthRetryDelay picks the wait before the next auth retry. A 429 means
// the API is explicitly asking us to slow down, unlike a timeout (which is
// just as likely transient on our end), so it scales more aggressively with
// the attempt count than every other error — otherwise a batch of proxies
// that all hit 429s keeps hammering the API at the same rate.
//
// Non-429 errors still scale with attempt, just gentler (1s/attempt instead
// of 5s/attempt, capped at 15s instead of 60s): a proven proxy's 9th retry
// got the exact same 0.5-10.5s jitter as its 1st, giving a chronically
// flaky proxy no extra breathing room as its failure streak grew.
func proxyAuthRetryDelay(err error, attempt int) time.Duration {
	if isRateLimitedError(err) {
		delay := time.Duration(attempt)*5*time.Second + time.Duration(mathrand.Intn(5000))*time.Millisecond
		if delay > 60*time.Second {
			delay = 60 * time.Second
		}
		return delay
	}
	delay := time.Duration(500+mathrand.Intn(3000)) * time.Millisecond
	if attempt > 1 {
		delay += time.Duration(attempt-1) * time.Second
	}
	if delay > 15*time.Second {
		delay = 15 * time.Second
	}
	return delay
}
