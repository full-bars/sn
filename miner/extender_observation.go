package miner

import "sync"

// extender_observation.go publishes the extender's latest reading process-wide.
//
// Every slot builds its own providerExtenderStatusListener, and only the direct
// slot's device carries an enabled extender role. A slot's listener is a local
// variable inside a worker, so without this the /metrics scrape and the control
// reply have no way to read the one reading that matters.
//
// Only an enabled reading is published. A proxy slot's listener reports the
// role off, and publishing that would overwrite the direct slot's real state on
// every callback, so the scrape would flap.

var extenderObservationLock sync.Mutex
var extenderObservationKnown bool
var extenderObservationState providerExtenderObservation

// setProviderExtenderObservation records the latest enabled reading.
func setProviderExtenderObservation(observation providerExtenderObservation) {
	extenderObservationLock.Lock()
	defer extenderObservationLock.Unlock()
	extenderObservationState = observation
	extenderObservationKnown = true
}

// providerExtenderObservationLatest returns the latest enabled reading, or nil
// when no enabled extender has reported since this process started. Nil is what
// keeps the families absent rather than publishing a fabricated zero.
func providerExtenderObservationLatest() *providerExtenderObservation {
	extenderObservationLock.Lock()
	defer extenderObservationLock.Unlock()
	if !extenderObservationKnown {
		return nil
	}
	observation := extenderObservationState
	return &observation
}

// The metrics seam reads what the listener published.
func init() {
	metricsSeamExtender = providerExtenderObservationLatest
}
