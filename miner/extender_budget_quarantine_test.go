package miner

import (
	"os"
	"testing"
	"time"
)

func quarantineBudgetForTest(t *testing.T) time.Time {
	t.Helper()
	testExtenderState(t)
	path, err := providerStatePath(providerStoreFileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a bbolt database"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openProviderStoreForProcess(); err != nil {
		t.Fatal(err)
	}
	return time.Now().Truncate(time.Second)
}

func TestExtenderQuarantineHoldsEveryAccount(t *testing.T) {
	now := quarantineBudgetForTest(t)
	if allowed, _, _ := extenderActivationAllow(now, 2, extenderTestAccountA); allowed {
		t.Fatal("account A allowed inside quarantine")
	}
	if allowed, _, _ := extenderActivationAllow(now, 2, extenderTestAccountB); allowed {
		t.Fatal("account B allowed after account A adopted the hold")
	}
	pass := extenderActivationPass{V4: extenderActivationFamilyResult{Tried: true}}
	if _, spent := extenderActivationSpendSpare(now, extenderTestAccountB, pass); spent {
		t.Fatal("spare spent inside quarantine")
	}
	extenderStoreAsSecondHost()
	if allowed, _, _ := extenderActivationAllow(now, 2, extenderTestAccountB); allowed {
		t.Fatal("hold lost on reopening store")
	}
}

func TestExtenderQuarantineSuccessCannotClearHold(t *testing.T) {
	now := quarantineBudgetForTest(t)
	recordExtenderActivationSucceeded(extenderTestAccountA)
	if allowed, wait, _ := extenderActivationAllow(now.Add(time.Minute), 2, extenderTestAccountA); allowed || wait < extenderActivationWindow-2*time.Minute {
		t.Fatalf("success shortened hold: allowed=%v wait=%s", allowed, wait)
	}
}

func TestExtenderQuarantineExpiresAndCountsPosts(t *testing.T) {
	now := quarantineBudgetForTest(t)
	if allowed, _, _ := extenderActivationAllow(now, 2, extenderTestAccountA); allowed {
		t.Fatal("account A allowed inside quarantine")
	}
	if allowed, _, _ := extenderActivationAllow(now.Add(extenderActivationWindow-time.Second), 2, extenderTestAccountB); allowed {
		t.Fatal("account B allowed before deadline")
	}
	later := now.Add(extenderActivationWindow + time.Second)
	if allowed, _, used := extenderActivationAllow(later, 2, extenderTestAccountB); !allowed || used != 0 {
		t.Fatalf("after expiry: allowed=%v used=%d, want true, 0", allowed, used)
	}
	if allowed, _, used := extenderActivationAllow(later, 2, extenderTestAccountB); !allowed || used != 2 {
		t.Fatalf("second launch: allowed=%v used=%d, want true, 2", allowed, used)
	}
	if allowed, _, used := extenderActivationAllow(later, 2, extenderTestAccountB); allowed || used != 4 {
		t.Fatalf("third launch: allowed=%v used=%d, want false, 4", allowed, used)
	}
}

func TestExtenderQuarantineSpareCannotEscape(t *testing.T) {
	now := quarantineBudgetForTest(t)
	if allowed, _, _ := extenderActivationAllow(now, 2, extenderTestAccountA); allowed {
		t.Fatal("account A allowed inside quarantine")
	}
	pass := extenderActivationPass{V4: extenderActivationFamilyResult{Tried: true}}
	if family, spent := extenderActivationSpendSpare(now, extenderTestAccountB, pass); spent {
		t.Fatalf("account B spent quarantine spare on %s", family)
	}
}
