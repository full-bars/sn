package miner

import "sync"

// extender_observation.go publishes the extender's latest reading process-wide.
//
// Every slot builds its own providerExtenderStatusListener, and only the direct
// slot's device carries an enabled extender role. A slot's listener is a local
// variable inside a worker, so without this the /metrics scrape and the control
// reply have no way to read the one reading that matters.
//
// The published reading carries the listener it came from. That identity is
// what keeps a proxy slot, or a direct listener that has been replaced, from
// erasing the reading of the listener that is actually running.

var extenderObservationLock sync.Mutex
var extenderObservationKnown bool
var extenderObservationListener *providerExtenderStatusListener
var extenderObservationState providerExtenderObservation

// setProviderExtenderObservation records an enabled reading and the listener it
// came from. A newer direct listener replaces an older one, which is what
// `provider direct off` followed by `provider direct on` produces.
func setProviderExtenderObservation(listener *providerExtenderStatusListener, observation providerExtenderObservation) {
	extenderObservationLock.Lock()
	defer extenderObservationLock.Unlock()
	if listener != nil && listener.retired {
		return
	}
	extenderObservationListener = listener
	extenderObservationState = observation
	extenderObservationKnown = true
}

// retireProviderExtenderObservation stops this listener from publishing and
// drops its reading if it is the published one. A late callback that already
// started cannot store a reading after this returns.
func retireProviderExtenderObservation(listener *providerExtenderStatusListener) {
	extenderObservationLock.Lock()
	defer extenderObservationLock.Unlock()
	if listener != nil {
		listener.retired = true
	}
	if extenderObservationKnown && extenderObservationListener == listener {
		extenderObservationKnown = false
		extenderObservationListener = nil
		extenderObservationState = providerExtenderObservation{}
	}
}

// clearProviderExtenderObservation drops the published reading when it belongs
// to this listener.
//
// Nothing else would replace the last enabled reading when the role stops. The
// status watch exits on context cancellation, so `provider direct off` and a
// device teardown would otherwise leave the scrape claiming an extender that is
// no longer running. A listener that is not the published one clears nothing,
// so a proxy slot cannot erase the direct slot's reading.
func clearProviderExtenderObservation(listener *providerExtenderStatusListener) {
	extenderObservationLock.Lock()
	defer extenderObservationLock.Unlock()
	if !extenderObservationKnown || extenderObservationListener != listener {
		return
	}
	extenderObservationKnown = false
	extenderObservationListener = nil
	extenderObservationState = providerExtenderObservation{}
}

// providerExtenderObservationLatest returns the latest enabled reading, or nil
// when no extender has published since this process started. Nil is what keeps
// the families absent rather than publishing a fabricated zero.
func providerExtenderObservationLatest() *providerExtenderObservation {
	extenderObservationLock.Lock()
	defer extenderObservationLock.Unlock()
	if !extenderObservationKnown {
		return nil
	}
	observation := extenderObservationState
	return &observation
}

// resetProviderExtenderObservation drops the published reading whatever its
// owner. A test uses it so one test's extender cannot be read by the next.
func resetProviderExtenderObservation() {
	extenderObservationLock.Lock()
	defer extenderObservationLock.Unlock()
	extenderObservationKnown = false
	extenderObservationListener = nil
	extenderObservationState = providerExtenderObservation{}
}

// The metrics seam reads what the listener published.
func init() {
	metricsSeamExtender = providerExtenderObservationLatest
}
