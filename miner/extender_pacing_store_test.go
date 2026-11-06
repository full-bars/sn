package miner

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// The pacing comes back from the store as it went in: times, the address the record
// was issued for, and the carriers the standing post offered.
func TestExtenderPacingStoreRoundTrip(t *testing.T) {
	testExtenderState(t)
	store := extenderPacingStore{}
	now := time.Unix(1_800_000_000, 0).UTC()
	saved := &connect.ExtenderActivationPacingRecord{
		PublicKey: []byte("an extender key of thirty-two b."),
		Families: map[int]connect.ExtenderActivationPacing{
			4: {
				State:                connect.ExtenderPacingActivated,
				RecordExpireTime:     now.Add(25 * time.Hour),
				LastAttemptAt:        now,
				NextAllowedAttemptAt: now.Add(24 * time.Hour),
				Ip:                   netip.MustParseAddr("198.51.100.7"),
				PostedCarriers:       []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns},
				PostedDnsPorts:       []int{53, 5353},
			},
			// a family that holds no record: every time and the address are zero
			6: {State: connect.ExtenderPacingUnactivated},
		},
	}
	if err := store.SavePacing(saved); err != nil {
		t.Fatalf("save: %s", err)
	}
	loaded, err := store.LoadPacing()
	if err != nil || loaded == nil {
		t.Fatalf("load = %+v, %v", loaded, err)
	}
	if string(loaded.PublicKey) != string(saved.PublicKey) || len(loaded.Families) != 2 {
		t.Fatalf("loaded = %+v", loaded)
	}
	v4 := loaded.Families[4]
	if v4.State != connect.ExtenderPacingActivated ||
		!v4.RecordExpireTime.Equal(now.Add(25*time.Hour)) ||
		!v4.LastAttemptAt.Equal(now) ||
		!v4.NextAllowedAttemptAt.Equal(now.Add(24*time.Hour)) ||
		v4.Ip != netip.MustParseAddr("198.51.100.7") ||
		len(v4.PostedCarriers) != 2 || v4.PostedCarriers[1] != connect.ExtenderCarrierDns ||
		len(v4.PostedDnsPorts) != 2 || v4.PostedDnsPorts[1] != 5353 {
		t.Fatalf("v4 = %+v", v4)
	}
	v6 := loaded.Families[6]
	if v6.State != connect.ExtenderPacingUnactivated || v6.Ip.IsValid() ||
		!v6.RecordExpireTime.IsZero() || v6.PostedCarriers != nil {
		t.Fatalf("v6 = %+v", v6)
	}
}

// No saved pacing is not an error: the loop starts as a new process does.
func TestExtenderPacingStoreLoadsNothingBeforeASave(t *testing.T) {
	testExtenderState(t)
	loaded, err := extenderPacingStore{}.LoadPacing()
	if loaded != nil || err != nil {
		t.Fatalf("load = %+v, %v, want nothing", loaded, err)
	}
}

// A store handed to the hotswap candidate belongs to it: a load or a save here
// would take the bbolt lock its next write waits on, or report a write that did not
// land. Both say so, and the activation loop keeps its pacing in memory.
func TestExtenderPacingStoreReleasedToTheHotswapCandidate(t *testing.T) {
	testExtenderState(t)
	store := extenderPacingStore{}
	if err := store.SavePacing(&connect.ExtenderActivationPacingRecord{PublicKey: []byte("k")}); err != nil {
		t.Fatalf("save before the release: %s", err)
	}
	releaseProviderStore()
	if _, err := store.LoadPacing(); !errors.Is(err, errProviderStoreReleased) {
		t.Fatalf("load after the release = %v, want %v", err, errProviderStoreReleased)
	}
	if err := store.SavePacing(&connect.ExtenderActivationPacingRecord{PublicKey: []byte("k")}); !errors.Is(err, errProviderStoreReleased) {
		t.Fatalf("save after the release = %v, want %v", err, errProviderStoreReleased)
	}
}

// An unreadable record is an error, not an empty pacing, so the loop logs it, and the
// next save replaces it.
func TestExtenderPacingStoreReplacesAnUnreadableRecord(t *testing.T) {
	testExtenderState(t)
	if err := providerStoreInstance().save(providerStoreBucketState, providerStoreKeyExtenderPacing, "not a pacing record"); err != nil {
		t.Fatalf("seed: %s", err)
	}
	store := extenderPacingStore{}
	if loaded, err := store.LoadPacing(); err == nil || loaded != nil {
		t.Fatalf("load = %+v, %v, want an error", loaded, err)
	}
	if err := store.SavePacing(&connect.ExtenderActivationPacingRecord{PublicKey: []byte("k")}); err != nil {
		t.Fatalf("save: %s", err)
	}
	if loaded, err := store.LoadPacing(); err != nil || loaded == nil || string(loaded.PublicKey) != "k" {
		t.Fatalf("load after the replacement = %+v, %v", loaded, err)
	}
}

// The pacing never touches the budget's record, and the budget's never the pacing's.
func TestExtenderPacingStoreKeepsItsOwnKey(t *testing.T) {
	testExtenderState(t)
	if allow, _, _ := extenderActivationAllow(time.Now(), postsPerDualStackPass, extenderTestAccountA); !allow {
		t.Fatal("the first start was refused")
	}
	if err := (extenderPacingStore{}).SavePacing(&connect.ExtenderActivationPacingRecord{PublicKey: []byte("k")}); err != nil {
		t.Fatalf("save: %s", err)
	}
	record, err := readExtenderActivationRecord(extenderTestAccountA)
	if err != nil || len(record.Accounts[extenderTestAccountA].Attempts) != postsPerDualStackPass {
		t.Fatalf("budget record = %+v, %v: the pacing save disturbed it", record, err)
	}
}

func pacedFamily(now time.Time, hold bool, validRecord bool, next time.Duration) connect.ExtenderActivationPacing {
	pacing := connect.ExtenderActivationPacing{
		State:                connect.ExtenderPacingFailed,
		NextAllowedAttemptAt: now.Add(next),
		Hold:                 hold,
	}
	if validRecord {
		pacing.State = connect.ExtenderPacingActivated
		pacing.RecordExpireTime = now.Add(25 * time.Hour)
	}
	return pacing
}

// A family is held back only while its next attempt is ahead and either a hold or the
// lack of a record keeps it there. A valid record with no hold posts once at start.
func TestExtenderPacingHeldFamilies(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name     string
		families map[int]connect.ExtenderActivationPacing
		held     int
	}{
		{"nothing stored", nil, 0},
		{"a hold ahead", map[int]connect.ExtenderActivationPacing{4: pacedFamily(now, true, false, time.Hour)}, 1},
		{"a hold beside a valid record", map[int]connect.ExtenderActivationPacing{4: pacedFamily(now, true, true, time.Hour)}, 1},
		{"a backoff with no record", map[int]connect.ExtenderActivationPacing{4: pacedFamily(now, false, false, time.Hour)}, 1},
		{"a valid record, no hold: posts once", map[int]connect.ExtenderActivationPacing{4: pacedFamily(now, false, true, 19*time.Hour)}, 0},
		{"a hold that has run out", map[int]connect.ExtenderActivationPacing{4: pacedFamily(now, true, false, -time.Minute)}, 0},
		{"a hold at the instant", map[int]connect.ExtenderActivationPacing{4: pacedFamily(now, true, false, 0)}, 0},
		{"both families held", map[int]connect.ExtenderActivationPacing{
			4: pacedFamily(now, true, false, time.Hour),
			6: pacedFamily(now, false, false, time.Hour),
		}, 2},
		{"one held, one posting", map[int]connect.ExtenderActivationPacing{
			4: pacedFamily(now, true, false, time.Hour),
			6: pacedFamily(now, false, true, 19*time.Hour),
		}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			record := &connect.ExtenderActivationPacingRecord{Families: c.families}
			if held := extenderPacingHeldFamilies(record, now); held != c.held {
				t.Fatalf("held = %d, want %d", held, c.held)
			}
		})
	}
	if held := extenderPacingHeldFamilies(nil, now); held != 0 {
		t.Fatalf("held of no record = %d", held)
	}
}

// The startup decision charges the posts the launch will make, and no more: a family
// the stored pacing holds back posts nothing, so it is not charged (F5). A family that
// posts once from a valid record is.
func TestExtenderGateChargesOnlyThePostsPacingWillMake(t *testing.T) {
	cases := []struct {
		name     string
		families map[int]connect.ExtenderActivationPacing
		charged  int
	}{
		{"no pacing: every family", nil, postsPerDualStackPass},
		{"one family held", map[int]connect.ExtenderActivationPacing{4: pacedFamily(time.Now(), true, false, time.Hour)}, postsPerDualStackPass - 1},
		{"every family held", map[int]connect.ExtenderActivationPacing{
			4: pacedFamily(time.Now(), true, false, time.Hour),
			6: pacedFamily(time.Now(), true, false, time.Hour),
		}, 0},
		{"valid records post once and are charged", map[int]connect.ExtenderActivationPacing{
			4: pacedFamily(time.Now(), false, true, 19*time.Hour),
			6: pacedFamily(time.Now(), false, true, 19*time.Hour),
		}, postsPerDualStackPass},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			testExtenderState(t)
			writeAccountToken(t, extenderTestAccountA)
			if c.families != nil {
				if err := (extenderPacingStore{}).SavePacing(&connect.ExtenderActivationPacingRecord{
					PublicKey: []byte("k"),
					Families:  c.families,
				}); err != nil {
					t.Fatalf("seed the pacing: %s", err)
				}
			}
			if !newExtenderActivationGate().decideAtStart() {
				t.Fatal("the start was refused")
			}
			record, err := readExtenderActivationRecord(extenderTestAccountA)
			if err != nil {
				t.Fatalf("read the budget: %s", err)
			}
			if got := len(record.Accounts[extenderTestAccountA].Attempts); got != c.charged {
				t.Fatalf("charged %d posts, want %d", got, c.charged)
			}
		})
	}
}

// A pacing that cannot be read leaves the charge as it was.
func TestExtenderGateChargesInFullWhenThePacingIsUnreadable(t *testing.T) {
	testExtenderState(t)
	if err := providerStoreInstance().save(providerStoreBucketState, providerStoreKeyExtenderPacing, "not a pacing record"); err != nil {
		t.Fatalf("seed: %s", err)
	}
	if got := pacedActivationPosts(postsPerDualStackPass, time.Now()); got != postsPerDualStackPass {
		t.Fatalf("posts = %d, want the full charge", got)
	}
}
