package clientauth

// identity_outcome.go names how a slot came to hold the client identity it
// authenticates with. The credential a slot ends up with looks the same
// whichever path produced it, so the registration path is the only place that
// knows whether the identity was kept, created, or replaced: a caller that has
// to report a start's identity health is told, not left to infer it from the
// token or from a log line.

import "errors"

// IdentityOutcome is one attempt's decision about a slot's client identity.
type IdentityOutcome uint8

const (
	// IdentityOutcomeUnobserved is the zero value: the attempt failed before
	// it read or created a credential, so it decided nothing about identity.
	IdentityOutcomeUnobserved IdentityOutcome = iota
	// IdentityOutcomeReused is a saved credential the platform accepted. The
	// identity is unchanged, which is not a heal and deserves no event.
	IdentityOutcomeReused
	// IdentityOutcomeMintedNew is a client registered where this slot had no
	// saved identity at all: a first start.
	IdentityOutcomeMintedNew
	// IdentityOutcomeMintedReplacement is a client registered in place of a
	// saved identity this slot could no longer present.
	IdentityOutcomeMintedReplacement
	// IdentityOutcomeRecovered is a retained registration operation completed,
	// so the identity is the one the server already bound to this slot and no
	// new client was created.
	IdentityOutcomeRecovered
	// IdentityOutcomeRevoked is the platform refusing this slot's identity.
	IdentityOutcomeRevoked
)

// IdentityObserver receives one attempt's identity decision and, when the
// decision has one, the reason for it. It runs on the registration caller's
// goroutine, so it must not block.
type IdentityObserver func(outcome IdentityOutcome, reason string)

// IsClientRevoked reports whether err is the refusal that stands: the platform
// refused the client this slot owns. A rejected saved credential is recorded
// under the same code and is replaced on the next attempt; a versioned
// operation the platform revoked is not replaced at all.
func IsClientRevoked(err error) bool {
	var refused *RegistrationRefusedError
	return errors.As(err, &refused) && refused.Code == "client_revoked"
}
