package miner

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func pprofGetStatus(t *testing.T, addr string) (int, error) {
	t.Helper()
	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + addr + "/debug/pprof/goroutine?debug=1")
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	return response.StatusCode, nil
}

func waitForProfilingAddr(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if addr := profilingAddr(); addr != "" {
			return addr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the diagnostics listener did not bind")
	return ""
}

func TestValidPprofValue(t *testing.T) {
	for _, value := range []string{"on", "off", " ON ", "127.0.0.1:6060", "[::1]:6060", "127.0.0.1:0"} {
		if !validPprofValue(value) {
			t.Fatalf("%q should be valid", value)
		}
	}
	for _, value := range []string{"", "yes", "0.0.0.0:6060", "10.0.0.1:6060", "localhost:6060", "127.0.0.1", "127.0.0.1:99999"} {
		if validPprofValue(value) {
			t.Fatalf("%q should be invalid", value)
		}
	}
}

func TestPprofControlKeyIsRegistered(t *testing.T) {
	if !controlKeys["pprof"] || !controlLiveEffectKeys["pprof"] {
		t.Fatal("pprof must be a persisted key with a live effect")
	}
	if controlLiveDefaults["pprof"] != "on" {
		t.Fatalf("the default is %q, want on", controlLiveDefaults["pprof"])
	}
	for _, value := range []string{"on", "off", "127.0.0.1:6060"} {
		if err := validateControlValue("pprof", value); err != nil {
			t.Fatalf("%q: %v", value, err)
		}
	}
	if validateControlValue("pprof", "0.0.0.0:6060") == nil {
		t.Fatal("a non-loopback address was accepted")
	}
}

func TestApplyPprofModeOnOffAndMove(t *testing.T) {
	t.Setenv(envProfiling, "")
	ctx, cancel := context.WithCancel(context.Background())
	profilingRunCtx.Store(profilingCtxBox{ctx: ctx})
	_ = stopProfiling()
	t.Cleanup(func() { cancel(); _ = stopProfiling() })

	if err := applyPprofMode("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	first := waitForProfilingAddr(t)
	if !strings.HasPrefix(first, "127.0.0.1:") {
		t.Fatalf("bound to %s, want loopback", first)
	}
	if status, err := pprofGetStatus(t, first); err != nil || status != http.StatusOK {
		t.Fatalf("GET %s = %d, %v", first, status, err)
	}
	if err := applyPprofMode("off"); err != nil {
		t.Fatal(err)
	}
	if profilingAddr() != "" {
		t.Fatal("listener still bound after off")
	}
	if _, err := pprofGetStatus(t, first); err == nil {
		t.Fatal("the old address still answers after off")
	}
	if err := applyPprofMode("bogus"); err == nil {
		t.Fatal("an invalid value was accepted")
	}
	if err := applyPprofMode("on"); err != nil {
		t.Fatal(err)
	}
	if got := waitForProfilingAddr(t); got == "" {
		t.Fatal("no address after turning it back on")
	}
}

func TestApplyPprofModeEnvOverrideWins(t *testing.T) {
	t.Setenv(envProfiling, "127.0.0.1:0")
	_ = stopProfiling()
	if err := applyPprofMode("on"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if profilingAddr() != "" {
		t.Fatal("the listener started although URNETWORK_PPROF decides the address")
	}
}
