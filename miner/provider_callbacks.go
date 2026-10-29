// These are the actual provide authentication and key callbacks. Required file
// effects retain their prior order and cancellation; logging is only an offer.
package miner

import (
	"context"
	"crypto/rand"
	"hash/fnv"
	"strconv"
	"sync"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"

	"github.com/urfoundation/sn/clientauth"
)

// SDK refresh/logout callbacks may share a dispatch worker. They never wait on
// diagnostic delivery or join their own API/device. The first file fault stays
// available to the enclosing provider after callback ownership is released.
type providerAuthenticationCallbacks struct {
	stateLock      sync.Mutex
	firstErr       error
	diagnostics    *providerDiagnostics
	provider       uint64
	clientJwtPath  string
	networkJwtPath string
	// identityKey is the slot's client JWT store key (jwtStoreKey). A
	// platform-rejected credential also evicts the hot-restart store entry so
	// a restart cannot hand the same poisoned identity straight back (the v33
	// "Delete on rejection": provider/provider_auth.go watchReusedIdentity...).
	identityKey string
	cancel      context.CancelFunc
	custody     *clientauth.ProviderClientKeyOwner
}

// Required token persistence precedes its existing global cancellation policy.
func (self *providerAuthenticationCallbacks) JwtRefreshed(jwt string) {
	var err error
	if self.custody != nil {
		err = self.custody.PersistClientJwt(self.clientJwtPath, jwt)
	} else {
		err = clientauth.WriteToken(self.clientJwtPath, jwt)
	}
	if err != nil {
		self.retainFailure(err)
		self.cancel()
		self.diagnostics.observe(providerJwtSaveFailed, self.provider, true, err, 0, nil)
	}
}

// A rejected credential is tombstoned before requesting shutdown. A failed
// tombstone is retained, without blocking cancellation on an output write.
func (self *providerAuthenticationCallbacks) AuthLogout() {
	var err error
	if self.custody != nil {
		err = self.custody.RejectClientJwt(self.clientJwtPath)
	} else {
		err = clientauth.MarkRejected(self.clientJwtPath, self.networkJwtPath)
	}
	if err != nil {
		self.retainFailure(err)
	}
	// Fork hook: a rejected credential must not be reused on the next start
	// either. The custody tombstone already blocks the durable-registration
	// path; evicting the hot-restart store entry is what stops provideClientJWT
	// from handing the same poisoned identity straight back.
	if self.identityKey != "" {
		if delErr := loadGlobalClientJWTStore().Delete(self.identityKey); delErr != nil {
			tlog("⚠️ [jwt-store] failed to evict rejected identity for %s: %v\n", self.identityKey, delErr)
		}
	}
	self.cancel()
	if err != nil {
		self.diagnostics.observe(providerRejectionSaveFailed, self.provider, true, err, 0, nil)
	}
	self.diagnostics.observe(providerAuthenticationRejected, self.provider, true, nil, 0, nil)
}

// Retain one original error without formatting or growing a callback history.
func (self *providerAuthenticationCallbacks) retainFailure(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.firstErr == nil {
		self.firstErr = err
	}
}

// The owner reads this after the API callback lifecycle has joined.
func (self *providerAuthenticationCallbacks) failure() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.firstErr
}

// These key saves keep their existing best-effort startup semantics. Each real
// failure remains visible as its own closed event; output never gates serving.
func persistProviderKeyMaterial(output *providerDiagnostics, provider uint64, identities providerSlotIdentities, proxySettings *connect.ProxySettings, keyMaterial *sdk.DeviceLocalKeyMaterial) {
	if seed := keyMaterial.GetClientKeySeed(); len(seed) > 0 {
		if err := writeProviderClientKeySeed(seed); err != nil {
			output.observe(providerClientKeySaveFailed, provider, true, err, 0, nil)
		}
	}
	persistProviderAuxiliaryKeyMaterial(output, provider, identities, proxySettings, keyMaterial)
}

// Device-generated TLS/extender state cannot overwrite the already owned and
// fsynced client key that selected the durable registration operation.
func persistProviderAuxiliaryKeyMaterial(output *providerDiagnostics, provider uint64, identities providerSlotIdentities, proxySettings *connect.ProxySettings, keyMaterial *sdk.DeviceLocalKeyMaterial) {
	certPem, keyPem := keyMaterial.GetProvideTlsCertificatePem(), keyMaterial.GetProvideTlsPrivateKeyPem()
	if len(certPem) > 0 && len(keyPem) > 0 {
		if err := writeProviderTlsCertAndKey(certPem, keyPem); err != nil {
			output.observe(providerTlsKeySaveFailed, provider, true, err, 0, nil)
		}
	}
	if seed := keyMaterial.GetExtenderKeySeed(); len(seed) > 0 {
		if err := writeProviderExtenderKeySeed(identities, proxySettings, seed); err != nil {
			output.observe(providerExtenderKeySaveFailed, provider, true, err, 0, nil)
		}
	}
}

// Derive only the availability fact; neither public identity nor private seed
// enters a callback log record. This remains independent of persistence success.
func observeProviderExtenderIdentity(output *providerDiagnostics, provider uint64, seed []byte) {
	if len(seed) == 0 {
		return
	}
	if _, err := connect.ExtenderPublicKeyFromSeed(seed); err != nil {
		output.observe(providerExtenderIdentityInvalid, provider, true, err, 0, nil)
		return
	}
	output.observe(providerIdentityReady, provider, true, nil, 0, nil)
}

// Only changed scalar facts are offered. SDK error/address strings are never
// formatted or retained, and exporter calls occur after the state lock releases.
type providerExtenderStatusListener struct {
	stateLock sync.Mutex
	observed  bool
	last      providerExtenderObservation
	// Remember every observed attempt so later redelivery cannot charge it twice.
	failureSeen map[string]bool
	// attemptScope makes an attempt's identity this listener's own. The status
	// names an attempt only by its millisecond, which two devices of one
	// account can share.
	attemptScope string
	// standing is the error the status has kept reporting, and standingTime
	// the attempt time this listener names it by (refusalTime).
	standing        bool
	standingDetail  uint64
	standingRefused bool
	standingTime    int64
	failureMu       sync.Mutex
	diagnostics     *providerDiagnostics
	provider        uint64
	// account is the sign-in account this slot provides under
	// (extenderActivationUserId), which is the identity the operator's
	// ExtenderActivateRateLimit counts and the key the budget records a
	// refusal or an activation against. The client id would be the wrong key:
	// a heal mints a new client for the same account.
	account       string
	accountPinned bool
}

// The provider borrows its enclosing run's explicitly supplied output budget.
func newProviderExtenderStatusListener(output *providerDiagnostics, provider uint64, account string) *providerExtenderStatusListener {
	return &providerExtenderStatusListener{diagnostics: output, provider: provider, account: account, attemptScope: rand.Text()}
}

// currentAccount is the account this observation is charged to. It resolves the
// sign-in token at observation time rather than trusting the key the listener
// was built with: the listener can be constructed before the token is readable,
// and then a refusal and the success that clears it would land on the
// placeholder instead of the account the operator is counting. The captured
// account stays as the fallback for a caller that has no token (tests, and a
// run whose token is still being written).
func (self *providerExtenderStatusListener) currentAccount() string {
	if self.accountPinned {
		return self.account
	}
	if account := extenderActivationUserId(); account != "" {
		self.stateLock.Lock()
		if !self.accountPinned {
			self.account, self.accountPinned = account, true
		}
		account = self.account
		self.stateLock.Unlock()
		return account
	}
	return self.account
}

// A nil observation is unknown, separate from a known disabled extender.
func (self *providerExtenderStatusListener) ExtenderProvideStatusChanged(status *sdk.ExtenderProvideStatus) {
	self.failureMu.Lock()
	defer self.failureMu.Unlock()
	observation := providerExtenderObservation{}
	if status != nil {
		observation = providerExtenderObservation{
			Known:             true,
			Enabled:           status.Enabled,
			Listening:         status.Listening,
			ListenFailed:      status.ListenError != "",
			ActivatedV4:       status.ActivatedV4,
			ActivatedV6:       status.ActivatedV6,
			ActivationFailed:  status.LastActivationError != "",
			ActivationTime:    status.LastActivationTime,
			ActivationRefused: status.LastActivationRefused,
			Revoked:           status.RevokedTime != 0,
		}
	}
	var detail uint64
	if status != nil {
		detail = activationDetailDigest(status.LastActivationError)
	}
	var attempt string
	changed, failed := func() (bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		changed := !self.observed || self.last != observation
		self.observed, self.last = true, observation
		// A failure is the attempt's, not the observation's. The error stays
		// set while the attempt stands, so only an attempt this listener has
		// not handled yet is handled again: a standing error re-read beside
		// another changed fact is the same failure. An activated family does
		// not clear it, because the two per-family flags are independent.
		attempt = self.attemptKey(observation, detail)
		failed := observation.Known && observation.Enabled && observation.ActivationFailed && !self.failureSeen[attempt]
		if failed {
			if self.failureSeen == nil {
				self.failureSeen = make(map[string]bool)
			}
			self.failureSeen[attempt] = true
		}
		return changed, failed
	}()
	if changed || failed {
		// The operator budgets activation posts per account per hour and its
		// refusal already names the wait, so the wait is persisted here: the
		// activation loop's own backoff lives in memory and a restart would
		// otherwise post into the same refusal.
		if observation.Known && observation.Enabled {
			switch {
			case observation.ActivationFailed:
				// A failed family is decided before any activation: the two
				// per-family flags are independent, so a pass that activated
				// v4 and failed v6 carries both, and testing the activation
				// first would report that pass as a success, record no
				// refusal and strand the retry the failure earned.
				if failed {
					// The retry is funded before this failure's wait is
					// recorded, so the wait that decides is the one already
					// standing: a retry is not charged until it can run.
					if family, failedFamily := observation.activationPass().failedFamily(); failedFamily {
						holdExtenderSpare(self.currentAccount(), family)
					}
					self.chargeActivationFailure(observation, attempt)
				}
			case observation.ActivatedV4 || observation.ActivatedV6:
				// Reached only by a pass with no failed family, which is what
				// makes a success safe to record.
				holdExtenderSuccess(self.currentAccount())
			}
		}
	}
	flushExtenderRefusals()
	if changed {
		self.diagnostics.observe(providerExtenderObserved, self.provider, true, nil, 0, &observation)
	}
}

// chargeActivationFailure persists the wait a refusal implies, and records
// nothing for a failure the operator did not refuse. LastActivationError is set
// for every failed attempt, including one that never reached the operator, so
// it says a family is not activated and not why: a dns failure, a dial timeout
// and a reset all set it, while LastActivationRefused is set only by an answer
// that came back activated: false. Holding the next start for the former spends
// this host's budget on a wait the operator never asked for.
func (self *providerExtenderStatusListener) chargeActivationFailure(observation providerExtenderObservation, attempt string) {
	if !observation.ActivationRefused {
		tlog("[extender] activation failed without a refusal (v4 activated=%t, v6 activated=%t); the next attempt is not held\n", observation.ActivatedV4, observation.ActivatedV6)
		return
	}
	// Held for the process, not this listener, so admission sees it before
	// the store has kept it and the listener's end does not drop it.
	holdExtenderRefusal(self.currentAccount(), attempt)
}

// activationDetailDigest reduces the status's failure text to a number, so the
// text itself is neither kept nor logged.
func activationDetailDigest(text string) uint64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(text))
	return hash.Sum64()
}

// attemptKey names one activation attempt of this listener by what the attempt
// reported: its time (refusalTime) and a digest of its failure text. The
// per-family activated flags are not part of it, because they change on their
// own while the same refusal stands, and a key that followed them would charge
// that refusal again. Two distinct refusals that finish in one millisecond
// differ in their text. The caller holds stateLock.
func (self *providerExtenderStatusListener) attemptKey(observation providerExtenderObservation, detail uint64) string {
	return self.attemptScope + "." + strconv.FormatInt(self.refusalTime(observation, detail), 10) +
		"." + strconv.FormatUint(detail, 16) +
		".r=" + strconv.FormatBool(observation.ActivationRefused)
}

// refusalTime is the time that names the standing error. The status carries the
// latest attempt of either family and the newest error separately, so its time
// is the error's own only while no family is activated: a success activates its
// family and clears its error, a failure deactivates it, and revocation (which
// also deactivates) is excluded. Then a newer time is a newer failed attempt.
// Once a family is activated, a newer time can be that family's success while
// the other's refusal stands, and the status cannot tell that from the refusal
// repeating with the same text. That case keeps the time the error was first
// seen at: a repeated identical refusal beside an activated family is charged
// once, which costs a shorter wait at the next start, where charging every
// newer time would double the wait for a refusal that happened once. The caller
// holds stateLock.
func (self *providerExtenderStatusListener) refusalTime(observation providerExtenderObservation, detail uint64) int64 {
	if !observation.Known || !observation.Enabled || !observation.ActivationFailed {
		self.standing = false
		return observation.ActivationTime
	}
	ownTime := !observation.ActivatedV4 && !observation.ActivatedV6 && !observation.Revoked
	if ownTime || !self.standing || self.standingDetail != detail || self.standingRefused != observation.ActivationRefused {
		self.standing = true
		self.standingDetail, self.standingRefused = detail, observation.ActivationRefused
		self.standingTime = observation.ActivationTime
	}
	return self.standingTime
}

// activationPass is what the status says this pass did, per family. A pass
// posts v4, and posts v6 only where this host's own measurement says v6 is
// usable, which is the same count the budget charged the start.
func (observation providerExtenderObservation) activationPass() extenderActivationPass {
	// Status only: an unreadable measurement counts as no v6, which is the
	// same conservative answer hostActivationPosts gives.
	capability, _ := readHostCapability()
	return extenderActivationPass{
		V4: extenderActivationFamilyResult{Tried: true, Activated: observation.ActivatedV4},
		V6: extenderActivationFamilyResult{
			Tried:     capability.IPv6Usable,
			Activated: observation.ActivatedV6,
		},
	}
}
