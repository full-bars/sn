package miner

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/sdk"
)

type fakeStatusApi struct {
	result   *sdk.SnGetWalletResult
	err      error
	epoch    int64
	epochErr error
}

func (self *fakeStatusApi) SnEpochSyncWithContext(context.Context) (*sdk.SnEpochResult, error) {
	if self.epochErr != nil {
		return nil, self.epochErr
	}
	return &sdk.SnEpochResult{Epoch: self.epoch}, nil
}

func (self *fakeStatusApi) SnGetWalletSyncWithContext(context.Context) (*sdk.SnGetWalletResult, error) {
	return self.result, self.err
}

func statusResult(wallets ...*sdk.SnWallet) *sdk.SnGetWalletResult {
	list := sdk.NewSnWalletList()
	for _, wallet := range wallets {
		list.Add(wallet)
	}
	return &sdk.SnGetWalletResult{Wallets: list}
}

func runStatus(t *testing.T, api *fakeStatusApi) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := snWalletStatus(context.Background(), &out, api)
	return out.String(), err
}

func TestWalletStatusShowsAConsentInEffect(t *testing.T) {
	text, err := runStatus(t, &fakeStatusApi{epoch: 20, result: statusResult(&sdk.SnWallet{
		ColdkeySs58: testAliceAddress, ConsentScope: "network", FromEpoch: 10, ThroughEpoch: 65545,
		MappingGeneration: 2, MappingHash: "0xabc",
	})})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"network consent for the network", testAliceAddress, "epochs 10 through 65545", "generation 2", "in effect now", "receipt 0xabc"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "no consent is stored") {
		t.Fatal("a stored consent was reported as missing")
	}
}

func TestWalletStatusSaysWhenAConsentStartsOrEnded(t *testing.T) {
	wallet := &sdk.SnWallet{ColdkeySs58: testAliceAddress, ConsentScope: "network", FromEpoch: 25, ThroughEpoch: 30}
	text, _ := runStatus(t, &fakeStatusApi{epoch: 20, result: statusResult(wallet)})
	if !strings.Contains(text, "starts in 5 epoch(s)") {
		t.Fatalf("a future consent: %s", text)
	}
	text, _ = runStatus(t, &fakeStatusApi{epoch: 40, result: statusResult(wallet)})
	if !strings.Contains(text, ", ended") {
		t.Fatalf("an ended consent: %s", text)
	}
	text, _ = runStatus(t, &fakeStatusApi{epochErr: errors.New("down"), result: statusResult(wallet)})
	if strings.Contains(text, "in effect") || strings.Contains(text, "starts in") {
		t.Fatalf("an unknown epoch was guessed: %s", text)
	}
}

func TestWalletStatusSeparatesALoginProofWalletFromAConsent(t *testing.T) {
	text, err := runStatus(t, &fakeStatusApi{result: statusResult(&sdk.SnWallet{ColdkeySs58: testAliceAddress})})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "NOT a consent") || !strings.Contains(text, "no consent is stored") {
		t.Fatalf("a login-proof wallet was shown as paying:\n%s", text)
	}
}

func TestWalletStatusNoWalletAndErrors(t *testing.T) {
	text, err := runStatus(t, &fakeStatusApi{result: &sdk.SnGetWalletResult{}})
	if err != nil || !strings.Contains(text, "no wallet is set") {
		t.Fatalf("empty: %q %v", text, err)
	}
	if _, err := runStatus(t, &fakeStatusApi{err: errors.New("boom")}); err == nil {
		t.Fatal("a transport error was swallowed")
	}
	if _, err := runStatus(t, &fakeStatusApi{result: &sdk.SnGetWalletResult{Error: &sdk.SnError{Message: "denied\x1b[31m"}}}); err == nil || strings.Contains(err.Error(), "\x1b") {
		t.Fatalf("a server error was not reported plainly: %v", err)
	}
	if _, err := runStatus(t, &fakeStatusApi{}); err == nil {
		t.Fatal("a nil answer was accepted")
	}
}
