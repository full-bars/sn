package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenBSDBareCleanupRemovesCron(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "cron failure"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cron := filepath.Join(dir, "crontab")
			script := "#!/bin/sh\nif [ \"$1\" = -l ]; then\n  printf '%s\\n' '0 1 * * * update # urnet-tools auto-update urnetwork' '0 2 * * * other'\nelse\n  /bin/cat > \"$CRON_OUTPUT\"\nfi\n"
			if fail {
				script = "#!/bin/sh\nexit 1\n"
			}
			if err := os.WriteFile(cron, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(dir, "output")
			t.Setenv("PATH", dir)
			t.Setenv("CRON_OUTPUT", output)
			err := cleanupOpenBSDLifecycle(Provider{Binary: "/usr/local/bin/urnetwork"})
			if fail {
				if err == nil || !strings.Contains(err.Error(), "remove auto-update cron entry") {
					t.Fatalf("expected cron cleanup error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "0 2 * * * other\n" {
				t.Fatalf("unexpected crontab: %q", got)
			}
		})
	}
}
