package urnettools

import (
	"errors"
	"testing"
)

// restartProvider had no Windows branch, so an update or tuning change on
// Windows fell into the systemctl code. It must hand off to the supervisor-
// aware Windows restart and never reach systemctl.
func TestRestartProviderRoutesWindowsToSupervisorRestart(t *testing.T) {
	prevOS, prevFn := restartGOOS, restartProviderWindowsFn
	t.Cleanup(func() { restartGOOS, restartProviderWindowsFn = prevOS, prevFn })

	restartGOOS = "windows"
	want := errors.New("sentinel")
	called := 0
	restartProviderWindowsFn = func(Provider) error { called++; return want }

	// Unit and User set: the systemctl path would be taken if the Windows
	// route were skipped.
	err := restartProvider(Provider{Unit: "urnetwork.service", User: "someone", PID: 4242})
	if !errors.Is(err, want) {
		t.Fatalf("restartProvider = %v, want the Windows restart's error", err)
	}
	if called != 1 {
		t.Fatalf("windows restart called %d times, want 1", called)
	}
}
