package walletsign

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/urnetwork/sdk"
)

// snWalletStatusApi is the part of the sdk api the status reads.
type snWalletStatusApi interface {
	SnEpochSyncWithContext(context.Context) (*sdk.SnEpochResult, error)
	SnGetWalletSyncWithContext(context.Context) (*sdk.SnGetWalletResult, error)
}

// snWalletStatus is `provider wallet status`: what the server holds for this
// network, with the consent that pays settlement shown apart from a
// login-proof wallet, which release settlement never pays.
func snWalletStatus(ctx context.Context, out io.Writer, api snWalletStatusApi) error {
	result, err := api.SnGetWalletSyncWithContext(ctx)
	if err != nil {
		return fmt.Errorf("wallet status: %w", err)
	}
	if result == nil {
		return errors.New("wallet status: the platform returned no answer")
	}
	if result.Error != nil {
		return fmt.Errorf("wallet status: %s", snPlain(result.Error.Message))
	}
	var epoch *sdk.SnEpochResult
	if e, err := api.SnEpochSyncWithContext(ctx); err == nil {
		epoch = e
	}
	var entries []*sdk.SnWallet
	if result.Wallets != nil {
		for i := 0; i < result.Wallets.Len(); i += 1 {
			if wallet := result.Wallets.Get(i); wallet != nil {
				entries = append(entries, wallet)
			}
		}
	}
	if len(entries) == 0 && result.Wallet != nil {
		entries = []*sdk.SnWallet{result.Wallet}
	}
	if len(entries) == 0 {
		fmt.Fprintf(out, "no wallet is set for this network\n")
		return nil
	}
	paying := 0
	for _, entry := range entries {
		subject := "the network"
		if entry.ClientId != "" {
			subject = "client " + snPlain(entry.ClientId)
		}
		if entry.ConsentScope == "" {
			fmt.Fprintf(out, "login-proof wallet %s for %s (NOT a consent: release settlement does not pay it)\n", snPlain(entry.ColdkeySs58), subject)
			continue
		}
		paying += 1
		current := ""
		if epoch != nil && entry.FromEpoch > 0 {
			switch {
			case epoch.Epoch < entry.FromEpoch:
				current = fmt.Sprintf(", starts in %d epoch(s)", entry.FromEpoch-epoch.Epoch)
			case entry.ThroughEpoch > 0 && epoch.Epoch > entry.ThroughEpoch:
				current = ", ended"
			default:
				current = ", in effect now"
			}
		}
		fmt.Fprintf(out, "%s consent for %s: coldkey %s, epochs %d through %d (generation %d)%s\n",
			snPlain(entry.ConsentScope), subject, snPlain(entry.ColdkeySs58), entry.FromEpoch, entry.ThroughEpoch, entry.MappingGeneration, current)
		if entry.MappingHash != "" {
			fmt.Fprintf(out, "  receipt %s\n", snPlain(entry.MappingHash))
		}
	}
	if paying == 0 {
		fmt.Fprintf(out, "no consent is stored: sign one with `provider wallet sign <coldkey_ss58>`\n")
	}
	return nil
}
