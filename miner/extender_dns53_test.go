package miner

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestDetermineDns53State(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	state := newControlState()

	oldProbe := dns53ProbeFunc
	t.Cleanup(func() { dns53ProbeFunc = oldProbe })

	// 1. Disabled by setting
	if err := state.set("extender_udp53", "off"); err != nil {
		t.Fatal(err)
	}
	if got := determineDns53State(nil, nil, state); got != "disabled" {
		t.Fatalf("expected disabled, got %q", got)
	}
	_ = state.clear("extender_udp53")

	// 2. Not requested by env
	t.Setenv("URNETWORK_EXTENDER_DNS53", "off")
	if got := determineDns53State(nil, nil, state); got != "not_requested" {
		t.Fatalf("expected not_requested, got %q", got)
	}
	t.Setenv("URNETWORK_EXTENDER_DNS53", "on")

	// 3. Bound (53 in DnsPorts)
	detail := &providerExtenderDetail{DnsPorts: "4053, 53"}
	obs := &providerExtenderObservation{Known: true, Enabled: true, Listening: true}
	if got := determineDns53State(obs, detail, state); got != "bound" {
		t.Fatalf("expected bound, got %q", got)
	}

	// 4. Pending (not listening yet)
	detailWithout53 := &providerExtenderDetail{DnsPorts: "4053"}
	obsNotListening := &providerExtenderObservation{Known: true, Enabled: true, Listening: false}
	if got := determineDns53State(obsNotListening, detailWithout53, state); got != "pending" {
		t.Fatalf("expected pending, got %q", got)
	}

	// 5. Address in use
	dns53ProbeFunc = func() error {
		return &net.OpError{Op: "listen", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
	}
	if got := determineDns53State(obs, detailWithout53, state); got != "address_in_use" {
		t.Fatalf("expected address_in_use, got %q", got)
	}

	// 6. Permission denied
	dns53ProbeFunc = func() error {
		return &net.OpError{Op: "listen", Err: os.NewSyscallError("bind", syscall.EACCES)}
	}
	if got := determineDns53State(obs, detailWithout53, state); got != "permission_denied" {
		t.Fatalf("expected permission_denied, got %q", got)
	}

	// 7. Unknown
	dns53ProbeFunc = func() error {
		return errors.New("custom bind failure")
	}
	if got := determineDns53State(obs, detailWithout53, state); got != "unknown" {
		t.Fatalf("expected unknown, got %q", got)
	}
}

func TestExtenderDns53Metrics(t *testing.T) {
	t.Setenv("URNETWORK_STATE_DIR", t.TempDir())
	oldProbe := dns53ProbeFunc
	t.Cleanup(func() { dns53ProbeFunc = oldProbe })

	resetProviderExtenderObservation()
	t.Cleanup(resetProviderExtenderObservation)

	obs := providerExtenderObservation{
		Known:     true,
		Enabled:   true,
		Listening: true,
	}
	detail := providerExtenderDetail{
		DnsPorts: "4053",
	}

	dns53ProbeFunc = func() error {
		return &net.OpError{Op: "listen", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
	}

	setProviderExtenderObservation(nil, obs, detail)

	metrics := minerExtraMetrics()
	want := `urnet_extender_dns53_state{state="address_in_use"} 1`
	if !strings.Contains(metrics, want) {
		t.Fatalf("metrics missing %q:\n%s", want, metrics)
	}

	for _, s := range dns53States {
		metricLine := fmt.Sprintf(`urnet_extender_dns53_state{state="%s"}`, s)
		if !strings.Contains(metrics, metricLine) {
			t.Fatalf("metrics missing state %q:\n%s", s, metrics)
		}
	}
}
