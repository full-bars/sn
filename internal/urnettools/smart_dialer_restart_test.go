package urnettools

import (
	"path/filepath"
	"strings"
	"testing"
)

// The provider reports smart_dialer restart-required now that the probe loop is
// created per identity at start. The CLI used to answer any accepted write with
// "applied live via control socket", so `urnet-tools set smart-dialer on` told
// the operator the running provider had switched when only the next start will:
// a live enable cannot create a probe loop, so it would point selection at a
// measurement stream nothing feeds.
//
// `set smart-dialer off` takes the off-as-clear path (clear restores the unset
// state, which the provider applies as off at startup), so both paths are
// pinned here.
func TestSmartDialerSetAndClearSayRestartNotLive(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "provider.sock")
	srv := startMockControlServer(t, sockPath)
	defer srv.Close()
	srv.needsRestart = map[string]bool{"smart_dialer": true}

	p := Provider{StateDir: dir, Running: true}
	for _, tc := range []struct {
		key, value string
		wantAction string
	}{
		{"smart-dialer", "on", "smart-dialer set to on"},
		{"smart-dialer", "off", "smart-dialer cleared"},
	} {
		out := captureStdout(t, func() {
			if err := applySetOverride(p, tc.key, tc.value, false); err != nil {
				t.Errorf("applySetOverride(%s=%s): %v", tc.key, tc.value, err)
			}
		})
		if !strings.Contains(out, tc.wantAction) {
			t.Errorf("%s=%s: output does not report the change (%q):\n%s", tc.key, tc.value, tc.wantAction, out)
		}
		if !strings.Contains(out, "persisted; takes effect at the next start") {
			t.Errorf("%s=%s: output does not say the change is persisted and waits for a start:\n%s", tc.key, tc.value, out)
		}
		if !strings.Contains(out, "requires a restart to take effect") {
			t.Errorf("%s=%s: output does not require a restart:\n%s", tc.key, tc.value, out)
		}
		if strings.Contains(out, "applied live") {
			t.Errorf("%s=%s: output claims a live apply for a restart-required key:\n%s", tc.key, tc.value, out)
		}
	}
}
