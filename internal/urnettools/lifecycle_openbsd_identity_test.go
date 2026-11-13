//go:build openbsd

package urnettools

import "testing"

func TestOpenBSDServiceControlRejectsWrongProcess(t *testing.T) {
	oldPID, oldRun := openbsdServicePID, bsdServiceRunFn
	t.Cleanup(func() { openbsdServicePID, bsdServiceRunFn = oldPID, oldRun })
	openbsdServicePID = func() int { return 123 }
	calls := 0
	bsdServiceRunFn = func(string, ...string) ([]byte, error) { calls++; return nil, nil }
	for _, action := range []string{"stop", "restart", "start", "disable"} {
		for _, p := range []Provider{
			{PID: 456, Supervisor: "openbsd-rcd", Binary: "/usr/local/libexec/urnetwork-provider/bin/urnetwork"},
			{PID: 123, Binary: "/usr/local/libexec/urnetwork-provider/bin/urnetwork"},
			{Supervisor: "openbsd-rcd"},
		} {
			if err := bsdServiceControl(p, action); err == nil {
				t.Fatalf("%s accepted %+v", action, p)
			}
		}
	}
	if calls != 0 {
		t.Fatalf("invoked service command %d times", calls)
	}
	if err := openbsdRequireManaged(Provider{PID: 123, Supervisor: "openbsd-rcd"}); err != nil {
		t.Fatalf("matching service rejected: %v", err)
	}
	openbsdServicePID = func() int { return 0 }
	if err := openbsdRequireManaged(Provider{PID: 123, Supervisor: "openbsd-rcd"}); err == nil {
		t.Fatal("stale identity accepted")
	}
}
