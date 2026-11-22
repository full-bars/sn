package miner

// Durable UDP relay measurements for the check in proxy_udp_check.go.
//
// A UDP verdict decides H3 and the identity's client UDP path, so it is only
// worth something if it survives a restart: a restart must not start from zero,
// and a proxy that is known not to relay must not be admitted again just
// because the process was replaced. The snapshot lives in its own file next to
// v6-ledger.json (proxy_measurement_file.go holds the format) and never in
// proxy.state, which several writers rewrite whole.
//
// The file is a cache of evidence, not a source of identity: it holds results
// keyed by a hash of the proxy identity, never a raw address, a credential or
// an error. What makes the file safe to read twice and to hand to a successor:
//   - a load validates every record before publishing any of them, and a file
//     that is unreadable, corrupt, from another version, too large or too
//     populated is ignored as a whole with one log line;
//   - a restored verdict seeds the store, so the H3 gate answers at once and
//     the normal lifetime still makes the check due again;
//   - a write happens at most once per measurementFlushDelay, through a sorted
//     temporary file renamed into place, so a reader never sees a partial file;
//   - the view is bounded by entry count and by age, and a write that fails
//     leaves the view dirty for the next tick instead of dropping it.
//
// An interrupted check records nothing here: the store forgets it, so the
// snapshot keeps the last published verdict.

import (
	"time"
)

var (
	// measurementStoreMaxEntries bounds the restored view as well as the file.
	// It is a variable so a test can lower it.
	measurementStoreMaxEntries = measurementFileMaxEntries
	// measurementFlushDelay batches a burst of finished checks into one write.
	measurementFlushDelay = 2 * time.Second
)

// measurementEntryFor builds the record saved for one published result. Exit
// addresses that do not parse are left out rather than written as evidence.
func measurementEntryFor(key string, result proxyUDPResult, at time.Time) measurementFileEntry {
	entry := measurementFileEntry{
		Key:     measurementKey(key),
		Status:  result.Status,
		At:      at,
		TCPExit: result.TCPExit,
		UDPExit: result.UDPExit,
	}
	if entry.TCPExit != "" && !validMeasurementIP(entry.TCPExit) {
		entry.TCPExit = ""
	}
	if entry.UDPExit != "" && !validMeasurementIP(entry.UDPExit) {
		entry.UDPExit = ""
	}
	return entry
}

// enableMeasurementPersistence loads the snapshot before the store is used. A
// missing, unreadable or rejected file leaves persistence on with an empty
// view, so later results are still written.
func (self *proxyUDPCheckStore) enableMeasurementPersistence(path string, now time.Time) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.measurementPath = path
	self.measurements = map[string]measurementFileEntry{}
	if path == "" {
		return
	}
	entries, err := loadMeasurementFile(path)
	if err != nil {
		tlog("[proxy][udp] ignoring the measurement snapshot: %s\n", err)
		return
	}
	for _, entry := range entries {
		if entry.At.After(now) {
			// a stamp in the future cannot be evidence of a past check
			continue
		}
		self.measurements[entry.Key] = entry
	}
	self.pruneMeasurementsLocked(now)
}

// restoredEntryLocked returns the entry for key, seeding the store from the
// snapshot on first use so a restart answers the H3 gate at once. Called with
// the lock held.
func (self *proxyUDPCheckStore) restoredEntryLocked(key string) *proxyUDPCheckEntry {
	if entry := self.entries[key]; entry != nil {
		return entry
	}
	if len(self.measurements) == 0 {
		return nil
	}
	saved, ok := self.measurements[measurementKey(key)]
	if !ok {
		return nil
	}
	entry := &proxyUDPCheckEntry{
		result: proxyUDPResult{Status: saved.Status, TCPExit: saved.TCPExit, UDPExit: saved.UDPExit},
		at:     saved.At,
	}
	self.entries[key] = entry
	return entry
}

// pruneMeasurementsLocked drops records that can no longer decide anything and
// then the oldest beyond the entry bound. Called with the lock held.
func (self *proxyUDPCheckStore) pruneMeasurementsLocked(now time.Time) {
	if len(self.measurements) == 0 {
		return
	}
	for hash, entry := range self.measurements {
		if now.Sub(entry.At) > 2*proxyUDPResultTTL {
			delete(self.measurements, hash)
		}
	}
	for len(self.measurements) > measurementStoreMaxEntries {
		oldestHash := ""
		var oldest time.Time
		for hash, entry := range self.measurements {
			if oldestHash == "" || entry.At.Before(oldest) {
				oldestHash, oldest = hash, entry.At
			}
		}
		delete(self.measurements, oldestHash)
	}
}

// persistMeasurementLocked records a published result and schedules one
// batched write. It only touches memory: the check path holds the lock, so it
// must never wait for a disk. Called with the lock held.
func (self *proxyUDPCheckStore) persistMeasurementLocked(key string, result proxyUDPResult, at time.Time) {
	if self.measurementPath == "" {
		return
	}
	if self.measurements == nil {
		self.measurements = map[string]measurementFileEntry{}
	}
	entry := measurementEntryFor(key, result, at)
	self.measurements[entry.Key] = entry
	self.pruneMeasurementsLocked(at)
	self.measurementDirty = true
	if at.After(self.measurementPending) {
		self.measurementPending = at
	}
	if self.measurementKick == nil {
		self.measurementKick = make(chan struct{}, 1)
	}
	if !self.measurementWriting {
		self.measurementWriting = true
		self.measurementDone = make(chan struct{})
		go self.measurementWriter()
	}
}

// measurementWriter writes the view until nothing is left to write, then
// exits. At most one runs per store.
func (self *proxyUDPCheckStore) measurementWriter() {
	for {
		timer := time.NewTimer(measurementFlushDelay)
		select {
		case <-timer.C:
		case <-self.measurementKick:
			timer.Stop()
		}
		self.mu.Lock()
		dirty := self.measurementDirty
		self.measurementDirty = false
		path := self.measurementPath
		list := make([]measurementFileEntry, 0, len(self.measurements))
		for _, entry := range self.measurements {
			list = append(list, entry)
		}
		self.mu.Unlock()

		var err error
		if dirty && path != "" {
			err = saveMeasurementFile(path, list)
		}

		self.mu.Lock()
		if err != nil {
			// Keep the view dirty for the next published result rather than
			// retrying here: a store on a read-only disk must not spin, and a
			// shutdown must not wait on a write that cannot land.
			self.measurementDirty = true
			if !self.measurementFailed {
				tlog("[proxy][udp] could not write the measurement snapshot: %s\n", err)
			}
			self.measurementFailed = true
			self.measurementWriting = false
			close(self.measurementDone)
			self.measurementDone = nil
			self.mu.Unlock()
			return
		}
		self.measurementFailed = false
		if !self.measurementDirty {
			self.measurementWriting = false
			close(self.measurementDone)
			self.measurementDone = nil
			self.mu.Unlock()
			return
		}
		self.mu.Unlock()
	}
}

// flushMeasurements writes any pending record now and waits for the writer to
// finish. A store with persistence off returns at once.
func (self *proxyUDPCheckStore) flushMeasurements() {
	self.mu.Lock()
	done := self.measurementDone
	if done != nil {
		select {
		case self.measurementKick <- struct{}{}:
		default:
		}
	}
	self.mu.Unlock()
	if done != nil {
		<-done
	}
}

// stopMeasurementPersistence flushes the last records and turns the snapshot
// off, so a draining process writes once and stops.
func (self *proxyUDPCheckStore) stopMeasurementPersistence() {
	self.flushMeasurements()
	self.mu.Lock()
	self.measurementPath = ""
	self.mu.Unlock()
}

// noteAttemptLocked records that a check was dispatched for key and has not
// published yet. A check that is cancelled keeps its record, so the sweep
// orders that identity after the identities it has not tried. Called with the
// lock held.
func (self *proxyUDPCheckStore) noteAttemptLocked(key string, at time.Time) {
	if self.attempts == nil {
		self.attempts = map[string]time.Time{}
	}
	self.attempts[key] = at
	for len(self.attempts) > measurementStoreMaxEntries {
		oldestKey := ""
		var oldest time.Time
		for candidate, stamp := range self.attempts {
			if oldestKey == "" || stamp.Before(oldest) {
				oldestKey, oldest = candidate, stamp
			}
		}
		delete(self.attempts, oldestKey)
	}
}

// clearAttemptLocked forgets the dispatch record of key once a check has
// published, because the result's own stamp now drives its order. Called with
// the lock held.
func (self *proxyUDPCheckStore) clearAttemptLocked(key string) {
	delete(self.attempts, key)
}

// attemptAtLocked is when key was last dispatched without publishing.
// Called with the lock held.
func (self *proxyUDPCheckStore) attemptAtLocked(key string) time.Time {
	return self.attempts[key]
}

// pruneAttemptsLocked drops dispatch records that are older than any result
// could be, so the table follows the checks instead of growing. A record for an
// identity that has not checked yet is kept: it is what rotates that identity
// behind the ones the sweep has not tried. Called with the lock held.
func (self *proxyUDPCheckStore) pruneAttemptsLocked(now time.Time) {
	for key, stamp := range self.attempts {
		if now.Sub(stamp) > 2*proxyUDPResultTTL {
			delete(self.attempts, key)
		}
	}
}
