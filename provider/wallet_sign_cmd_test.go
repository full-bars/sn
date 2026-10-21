package provider

import (
	"testing"

	"github.com/docopt/docopt-go"
)

const testWalletSignColdkey = "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"

func TestWalletSignAndStatusUsageParses(t *testing.T) {
	opts, err := docopt.ParseArgs(mainUsage(), []string{"wallet", "sign", testWalletSignColdkey, "--port=9000", "--no-open"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if sign, _ := opts.Bool("sign"); !sign {
		t.Fatal("sign not selected")
	}
	if noOpen, _ := opts.Bool("--no-open"); !noOpen {
		t.Fatal("--no-open lost")
	}
	if port, _ := opts.String("--port"); port != "9000" {
		t.Fatalf("port %q", port)
	}
	if cold, _ := opts.String("<coldkey_ss58>"); cold != testWalletSignColdkey {
		t.Fatalf("coldkey %q", cold)
	}
	opts, err = docopt.ParseArgs(mainUsage(), []string{"wallet", "status"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := opts.Bool("status"); !status {
		t.Fatal("status not selected")
	}
	if hotkey, _ := opts.Bool("hotkey"); hotkey {
		t.Fatal("wallet status selected hotkey")
	}
	// wallet hotkey status stays the hotkey command
	opts, err = docopt.ParseArgs(mainUsage(), []string{"wallet", "hotkey", "status"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if hotkey, _ := opts.Bool("hotkey"); !hotkey {
		t.Fatal("wallet hotkey status lost its hotkey")
	}
}
