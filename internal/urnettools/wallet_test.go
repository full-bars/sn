package urnettools

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The wallet group: help renders from the cobra tree (never by running the
// tool), the fleet-node refusals guard the seed-bearing forms, and the
// delegation passes the provider's own flags through untouched.

func walletTestProvider() Provider {
	return Provider{
		Unit:    "urnetwork-native.service",
		User:    "urnet",
		Binary:  "/usr/bin/provider",
		Running: true,
	}
}

// walletWorkstationProvider is the non-fleet shape: a provider the operator
// runs from their own home, not a systemd service.
func walletWorkstationProvider() Provider {
	return Provider{
		Unit:    "manual",
		User:    "operator",
		Binary:  "/usr/bin/provider",
		Running: true,
	}
}

// withWalletSeams installs the test seams and restores them on cleanup.
func withWalletSeams(t *testing.T, providers []Provider, recorder *[][]string, scriptPath, readmePath string) {
	t.Helper()
	prevProviders := discoverProvidersForWallet
	prevDocker := discoverDockerForWallet
	prevSub := walletProviderSubcommand
	discoverProvidersForWallet = func() []Provider { return providers }
	discoverDockerForWallet = func() []Provider { return nil }
	if recorder != nil {
		walletProviderSubcommand = func(p Provider, args ...string) error {
			*recorder = append(*recorder, append([]string{}, args...))
			return nil
		}
	}
	t.Cleanup(func() {
		discoverProvidersForWallet = prevProviders
		discoverDockerForWallet = prevDocker
		walletProviderSubcommand = prevSub
	})
}

func TestWalletHelpRendersFromTheTree(t *testing.T) {
	if !strings.Contains(rootHelpMenu, "wallet status") {
		t.Fatal("the root help menu must list the wallet group")
	}
	root := buildRootCmd()
	wallet, _, err := root.Find([]string{"wallet"})
	if err != nil || wallet.Name() != "wallet" {
		t.Fatalf("wallet command not found: %v", err)
	}
	if wallet.Short == "" || wallet.Long == "" || wallet.Example == "" {
		t.Fatalf("wallet group must have Short/Long/Example: short=%q long=%q example=%q", wallet.Short, wallet.Long, wallet.Example)
	}
	for _, name := range []string{"status", "challenge", "set", "sign"} {
		var sub *cobra.Command
		for _, candidate := range wallet.Commands() {
			if candidate.Name() == name {
				sub = candidate
			}
		}
		if sub == nil {
			t.Fatalf("wallet %s missing", name)
		}
		if sub.Short == "" || sub.Long == "" || sub.Example == "" {
			t.Fatalf("wallet %s must have Short/Long/Example: short=%q long=%q example=%q", name, sub.Short, sub.Long, sub.Example)
		}
		if !strings.Contains(sub.Example, "urnet-tools wallet") {
			t.Fatalf("wallet %s Example must show real invocations, got %q", name, sub.Example)
		}
	}
	// the seed-file warning is loud in the Long text
	var setCmd *cobra.Command
	for _, candidate := range wallet.Commands() {
		if candidate.Name() == "set" {
			setCmd = candidate
		}
	}
	if !strings.Contains(setCmd.Long, "OWN WORKSTATION") || !strings.Contains(setCmd.Long, "never a fleet node") {
		t.Fatal("wallet set Long must carry the workstation-only warning")
	}
}

func TestWalletStatusDelegates(t *testing.T) {
	var calls [][]string
	withWalletSeams(t, []Provider{walletTestProvider()}, &calls, "", "")
	if err := cmdWalletStatus([]string{"--unit", "urnetwork-native.service"}); err != nil {
		t.Fatalf("wallet status: %v", err)
	}
	if len(calls) != 1 || strings.Join(calls[0], " ") != "wallet status" {
		t.Fatalf("delegation args = %v, want [wallet status]", calls)
	}
}

func TestWalletChallengeRequiresAColdkey(t *testing.T) {
	var calls [][]string
	withWalletSeams(t, []Provider{walletTestProvider()}, &calls, "", "")
	err := cmdWalletChallenge(nil)
	if err == nil || !strings.Contains(err.Error(), "coldkey") {
		t.Fatalf("wallet challenge with no coldkey must give a helpful error, got %v", err)
	}
	if len(calls) != 0 {
		t.Fatal("nothing may be delegated without a coldkey")
	}
	// with a coldkey the challenge delegates and passes provider flags through
	err = cmdWalletChallenge([]string{"5CvF...ss58", "--wallet-from-epoch", "51", "-f"})
	if err != nil {
		t.Fatalf("wallet challenge: %v", err)
	}
	joined := strings.Join(calls[0], " ")
	if !strings.HasPrefix(joined, "wallet challenge 5CvF...ss58") || !strings.Contains(joined, "--wallet-from-epoch 51") {
		t.Fatalf("delegation args = %q", joined)
	}
}

func TestWalletSetSeedFileRefusedOnFleetNode(t *testing.T) {
	var calls [][]string
	withWalletSeams(t, []Provider{walletTestProvider()}, &calls, "", "")
	err := cmdWalletSet([]string{"-f", "5CvF...ss58", "--coldkey_seed_file=/tmp/seed"})
	if err == nil || !strings.Contains(err.Error(), walletWorkstationFlag) {
		t.Fatalf("the seed-file form on a fleet node must refuse and name the workstation flag, got %v", err)
	}
	if len(calls) != 0 {
		t.Fatal("a refused set must not delegate")
	}
	// the explicit assertion allows it, and the tool flag is stripped before
	// the provider sees the args
	err = cmdWalletSet([]string{"-f", walletWorkstationFlag, "5CvF...ss58", "--coldkey_seed_file=/tmp/seed"})
	if err != nil {
		t.Fatalf("wallet set with the workstation assertion: %v", err)
	}
	joined := strings.Join(calls[0], " ")
	if joined != "wallet set 5CvF...ss58 --coldkey_seed_file=/tmp/seed" {
		t.Fatalf("delegation args = %q (the tool flag must not reach the provider)", joined)
	}
	// the message/signature form is untouched by the fleet rule
	calls = nil
	err = cmdWalletSet([]string{"-f", "5CvF...ss58", "--message=m", "--signature=0x00"})
	if err != nil {
		t.Fatalf("wallet set signed form: %v", err)
	}
	if len(calls) != 1 {
		t.Fatal("the signed form must delegate without the fleet gate")
	}
}

func TestWalletFleetDetectionViaProviderEnv(t *testing.T) {
	var calls [][]string
	// no discovered providers, but the provider runtime environment is present
	withWalletSeams(t, nil, &calls, "", "")
	t.Setenv("URNETWORK_PROFILE", "lowmem")
	if err := cmdWalletSet([]string{"-f", "5CvF...ss58", "--coldkey_seed_file=/tmp/seed"}); err == nil {
		t.Fatal("the provider environment marker must count as a fleet node")
	}
	if err := cmdWalletSign([]string{"5CvF...ss58"}); err == nil {
		t.Fatal("wallet sign must refuse on a fleet node")
	}
}

func TestWalletSignDelegatesToTheProviderSigningPage(t *testing.T) {
	var calls [][]string
	withWalletSeams(t, []Provider{walletWorkstationProvider()}, &calls, "", "")
	// -f skips the confirm prompt
	if err := cmdWalletSign([]string{"-f", "5CvF...ss58", "--no-open"}); err != nil {
		t.Fatalf("wallet sign: %v", err)
	}
	if len(calls) != 1 || strings.Join(calls[0], " ") != "wallet sign 5CvF...ss58 --no-open" {
		t.Fatalf("wallet sign must run 'provider wallet sign' with the arguments, got %v", calls)
	}
	// the address is optional: the account is chosen on the page
	calls = nil
	if err := cmdWalletSign([]string{"-f"}); err != nil {
		t.Fatalf("wallet sign without an address: %v", err)
	}
	if len(calls) != 1 || strings.Join(calls[0], " ") != "wallet sign" {
		t.Fatalf("wallet sign without an address must still delegate, got %v", calls)
	}
}
