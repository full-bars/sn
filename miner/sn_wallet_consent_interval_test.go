// The wallet-consent interval is a financial commitment that cannot be undone,
// so the interval rules are pinned here without a server: the default window,
// the allowed start, and both odd-interval refusals with the operator override.
package miner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/sdk"
)

// snWalletEpochStub answers the one call the interval makes; the other
// interface methods are unreachable from snWalletMappingInterval.
type snWalletEpochStub struct {
	epoch int64
	err   error
}

func (self *snWalletEpochStub) SnEpochSyncWithContext(context.Context) (*sdk.SnEpochResult, error) {
	if self.err != nil {
		return nil, self.err
	}
	return &sdk.SnEpochResult{Epoch: self.epoch}, nil
}

func (self *snWalletEpochStub) SnWalletMappingChallengeSyncWithContext(context.Context, *sdk.SnWalletMappingChallengeArgs) (*sdk.SnWalletMappingChallengeResult, error) {
	return nil, errors.New("the interval requests no challenge")
}

func (self *snWalletEpochStub) SnSetWalletSyncWithContext(context.Context, *sdk.SnSetWalletArgs) (*sdk.SnSetWalletResult, error) {
	return nil, errors.New("the interval sets no wallet")
}

func snWalletIntervalPtr(epoch int64) *int64 { return &epoch }

// A consent cannot be moved earlier and settlement never falls back to an
// earlier generation, so an interval that would leave epochs unpaid is refused
// unless the operator sets URNETWORK_WALLET_ALLOW_ODD_INTERVAL=1
// (v33 wallet_network_consent.go snConsentInterval, AU-13).
func TestSnWalletMappingIntervalRefusesOddIntervals(t *testing.T) {
	t.Setenv("URNETWORK_WALLET_ALLOW_ODD_INTERVAL", "")
	api := &snWalletEpochStub{epoch: 100}
	farFrom := int64(400)
	shortFrom := int64(100)
	for _, testCase := range []struct {
		from, through int64
		want          string
	}{
		{farFrom, farFrom + snWalletConsentSpan, "far beyond the current epoch 100"},
		{shortFrom, shortFrom + snWalletConsentSpan - 1, "not the full 65536"},
	} {
		_, _, err := snWalletMappingInterval(t.Context(), api, &snWalletProof{fromEpoch: &testCase.from, throughEpoch: &testCase.through})
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("interval %d..%d: want refusal %q, got %v", testCase.from, testCase.through, testCase.want, err)
		}
	}

	// The explicit override is the only way past both refusals, and it signs
	// the operator's bounds exactly.
	t.Setenv("URNETWORK_WALLET_ALLOW_ODD_INTERVAL", "1")
	for _, testCase := range []struct{ from, through int64 }{{farFrom, farFrom + snWalletConsentSpan}, {shortFrom, shortFrom + snWalletConsentSpan - 1}} {
		from, through, err := snWalletMappingInterval(t.Context(), api, &snWalletProof{fromEpoch: &testCase.from, throughEpoch: &testCase.through})
		if err != nil || from != testCase.from || through != testCase.through {
			t.Fatalf("override %d..%d changed to %d..%d: %v", testCase.from, testCase.through, from, through, err)
		}
	}
}

// Without --wallet-from-epoch the window is the next earning epoch through the
// protocol's finite span, and a start two epochs ahead is still allowed
// (v33 allows a start up to epoch+2, since only the consent's own generation
// bounds what the server accepts).
func TestSnWalletMappingIntervalDefaultWindowAndAllowedStart(t *testing.T) {
	api := &snWalletEpochStub{epoch: 100}
	from, through, err := snWalletMappingInterval(t.Context(), api, nil)
	if err != nil || from != 101 || through != 65636 || through-from != snWalletConsentSpan {
		t.Fatalf("default window = %d..%d: %v", from, through, err)
	}
	if _, _, err := snWalletMappingInterval(t.Context(), api, &snWalletProof{}); err != nil {
		t.Fatalf("a proof without explicit bounds is the default window: %v", err)
	}

	allowed := int64(102)
	allowedThrough := allowed + snWalletConsentSpan
	if gotFrom, gotThrough, err := snWalletMappingInterval(t.Context(), api, &snWalletProof{fromEpoch: &allowed, throughEpoch: &allowedThrough}); err != nil || gotFrom != allowed || gotThrough != allowedThrough {
		t.Fatalf("start %d (epoch+2) was refused: %d..%d, %v", allowed, gotFrom, gotThrough, err)
	}
	tooLate := int64(103)
	tooLateThrough := tooLate + snWalletConsentSpan
	if _, _, err := snWalletMappingInterval(t.Context(), api, &snWalletProof{fromEpoch: &tooLate, throughEpoch: &tooLateThrough}); err == nil {
		t.Fatalf("start %d beyond epoch+2 signed", tooLate)
	}

	// An unavailable epoch state names no window at all.
	if _, _, err := snWalletMappingInterval(t.Context(), &snWalletEpochStub{err: errors.New("epoch unavailable")}, nil); err == nil {
		t.Fatal("an unavailable epoch state produced a window")
	}
	if _, _, err := snWalletMappingInterval(t.Context(), &snWalletEpochStub{epoch: -1}, nil); err == nil {
		t.Fatal("a negative epoch produced a window")
	}
}
