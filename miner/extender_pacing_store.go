package miner

// extender_pacing_store.go keeps the extender activation loop's pacing in
// provider_state.db, so a restart resumes the cadence the record's validity set
// instead of posting every family at once.
//
// The activation budget (extender_budget.go) counts the posts the operator will
// have counted. The pacing is the other half: when each family may post next, and
// how its last attempt ended. It is one record under its own key of the state
// bucket, written by the loop as it paces and read once when the loop starts
// (connect.ExtenderActivationPacingStore). The store handle is the process-wide
// one, looked up on every call, so a store released to a hotswap candidate
// answers with errProviderStoreReleased and the loop keeps its pacing in memory
// instead of writing to a file the candidate owns.

import (
	"time"

	"github.com/urnetwork/connect"
)

// providerStoreKeyExtenderPacing is the record key of the pacing, beside the
// budget's providerStoreKeyExtenderActivation.
const providerStoreKeyExtenderPacing = "extender_pacing"

// extenderPacingStore is connect.ExtenderActivationPacingStore over the provider
// state store. It holds nothing: every call reads the process-wide handle.
type extenderPacingStore struct{}

var _ connect.ExtenderActivationPacingStore = extenderPacingStore{}

// LoadPacing returns the stored pacing, nil when none was saved. An unreadable
// record is an error, which starts the loop as a new process does; the next save
// replaces it.
func (extenderPacingStore) LoadPacing() (*connect.ExtenderActivationPacingRecord, error) {
	store, err := openProviderStoreForProcess()
	if err != nil {
		return nil, err
	}
	var record connect.ExtenderActivationPacingRecord
	found, err := store.load(providerStoreBucketState, providerStoreKeyExtenderPacing, &record)
	if err != nil || !found {
		return nil, err
	}
	return &record, nil
}

// SavePacing writes the pacing in one transaction. A released or closed store
// reports it, so the loop logs the loss instead of counting the pacing as kept.
func (extenderPacingStore) SavePacing(record *connect.ExtenderActivationPacingRecord) error {
	store, err := openProviderStoreForProcess()
	if err != nil {
		return err
	}
	return store.save(providerStoreBucketState, providerStoreKeyExtenderPacing, record)
}

// extenderPacingHeldFamilies counts the families of a stored pacing that will not
// post when a launch starts: the ones the loop holds back until their stored time.
// A family posts at start unless its next attempt is still ahead and either the
// operator's word (a hold) or the lack of a valid record keeps it there. A family
// with a valid record and no hold posts once, spread over the first hour, to hand
// the new process's carriers to the operator, so it is a post and is charged.
func extenderPacingHeldFamilies(record *connect.ExtenderActivationPacingRecord, now time.Time) int {
	if record == nil {
		return 0
	}
	held := 0
	for _, pacing := range record.Families {
		if !pacing.NextAllowedAttemptAt.After(now) {
			continue
		}
		if pacing.Hold || !pacing.RecordValid(now) {
			held++
		}
	}
	return held
}

// pacedActivationPosts is how many of the posts a launch would spend the stored
// pacing lets it make. The budget charges a launch up front, and a family the
// pacing holds back posts nothing, so charging it would spend the operator's
// window on a post that is not made. A store that cannot be read leaves the
// charge as it was. The stored key is not compared with the key the role will
// use: a changed identity under a held family undercharges by at most that
// family, inside the margin the budget keeps under the operator's six.
func pacedActivationPosts(posts int, now time.Time) int {
	record, err := extenderPacingStore{}.LoadPacing()
	if err != nil {
		return posts
	}
	return max(0, posts-extenderPacingHeldFamilies(record, now))
}
