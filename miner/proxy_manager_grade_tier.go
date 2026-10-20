package miner

// Ported from the 3.23-fix v33 provider's proxy_grade_tier.go (read-only source
// candidate/v33-ops). The letter-grade mapping and the tier rank live in
// proxy_manager_url.go (they were carried over with the P1(a) URL-cache port,
// which the merge ordering needs); this file carries the probe-result side of
// the tier model.

// gradeTierFor derives the tier for a probe result. An undecidable pass
// (no genuine verdict - cancelled, DNS-gutted, nothing asked) has NO tier:
// absence of evidence is not an F. A decidable pass is always graded,
// including a decidable 0.0 (the honeypot that answered nothing it was
// asked - that IS an F).
func gradeTierFor(res tableProbeResult) string {
	if !res.Decidable {
		return ""
	}
	return proxyGradeTier(res.Score)
}
