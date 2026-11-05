package urnettools

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// `urnet-tools state verify|identities` asks the running provider to inspect
// its own state store. Both are read-only and answered by the provider that
// owns the store; this tool never opens provider_state.db itself.

func newStateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "state <verify|identities> [target flags]",
		Short: "inspect the provider's state store (read-only, asked of the running provider)",
		Long: "verify reports the store's schema, whether it is open, whether its records decode, each " +
			"migration family's seal and the file-versus-store comparison. identities lists identity slots " +
			"by a hashed label with their latest outcome and event count. Neither repairs anything, and " +
			"neither prints a token, seed, proxy password or path. Anything other than \"clean\" (including " +
			"\"unchecked\", \"unavailable\" and \"locked\") makes verify exit non-zero.",
		Example:            "  urnet-tools state verify\n  urnet-tools state identities --unit urnetwork-native.service",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if hasHelpFlag(args) {
				return cmd.Help()
			}
			return cmdState(args, os.Stdout)
		},
	}
}

type stateFamilyReply struct {
	Name       string `json:"name"`
	Mode       string `json:"mode"`
	Seal       string `json:"seal"`
	Generation uint64 `json:"generation"`
	Decodable  string `json:"decodable"`
	Reconcile  string `json:"reconcile"`
	Status     string `json:"status"`
}

type stateVerifyReply struct {
	Status      string             `json:"status"`
	Store       string             `json:"store"`
	Schema      string             `json:"schema"`
	Records     int                `json:"records"`
	Undecodable int                `json:"undecodable"`
	Families    []stateFamilyReply `json:"families"`
}

type stateIdentitiesReply struct {
	Status      string `json:"status"`
	Events      int    `json:"events"`
	Undecodable int    `json:"undecodable"`
	Slots       []struct {
		Slot    string `json:"slot"`
		Outcome string `json:"outcome"`
		Events  int    `json:"events"`
	} `json:"slots"`
}

// stateOrUnchecked keeps an absent or empty status from reading as healthy.
func stateOrUnchecked(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unchecked"
	}
	return s
}

func cmdState(args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "verify" && args[0] != "identities") {
		return fmt.Errorf("usage: urnet-tools state <verify|identities> [target flags]")
	}
	action := args[0]
	t, rest, err := parseTargetFlags(args[1:])
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("state %s takes no arguments (got %q)", action, rest[0])
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	if p.StateDir == "" {
		return fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	resp, err := sendSocketRequest(filepath.Join(p.StateDir, "provider.sock"), controlRequest{Cmd: "state", Action: action})
	if err != nil {
		return err
	}
	if !resp.OK {
		if strings.HasPrefix(resp.Error, "unknown command") {
			return fmt.Errorf("provider %s is too old to know `state`; update it with `urnet-tools update`", providerLabel(p))
		}
		return fmt.Errorf("provider returned error: %s", resp.Error)
	}
	return renderState(action, resp.Value, out)
}

func renderState(action, raw string, out io.Writer) error {
	if action == "identities" {
		var r stateIdentitiesReply
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			return fmt.Errorf("unreadable state reply: %w", err)
		}
		status := stateOrUnchecked(r.Status)
		fmt.Fprintf(out, "identities: %s (%d slots, %d events, %d undecodable)\n", status, len(r.Slots), r.Events, r.Undecodable)
		for _, s := range r.Slots {
			fmt.Fprintf(out, "  %s  %-20s  %d event(s)\n", s.Slot, s.Outcome, s.Events)
		}
		if status != "clean" {
			return fmt.Errorf("identities listing is not clean: %s", status)
		}
		return nil
	}
	var r stateVerifyReply
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return fmt.Errorf("unreadable state reply: %w", err)
	}
	status := stateOrUnchecked(r.Status)
	fmt.Fprintf(out, "state: %s\n  store: %s   schema: %s   records: %d (%d undecodable)\n",
		status, stateOrUnchecked(r.Store), stateOrUnchecked(r.Schema), r.Records, r.Undecodable)
	for _, f := range r.Families {
		fmt.Fprintf(out, "  family %-10s mode=%s seal=%s decodable=%s reconcile=%s -> %s\n",
			f.Name, f.Mode, f.Seal, f.Decodable, stateOrUnchecked(f.Reconcile), stateOrUnchecked(f.Status))
	}
	fmt.Fprintln(out, "  read-only: nothing was repaired or changed")
	if status != "clean" {
		return fmt.Errorf("state is not clean: %s", status)
	}
	return nil
}
