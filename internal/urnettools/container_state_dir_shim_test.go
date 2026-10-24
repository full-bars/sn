//go:build !windows

package urnettools

import (
	"os"
	"path/filepath"
	"testing"
)

// TestContainerStateDirHonoursContainerOverride drives containerStateDir through
// a `docker` shim (the URNET_DOCKER_BIN seam) instead of a real daemon.
//
// The provider INSIDE the container resolves its state dir the same way the host
// miner does — URNETWORK_STATE_DIR when it runs with one, else $HOME/.urnetwork.
// idle-update reads billable_rate from whatever this returns, so reading only
// HOME made it `docker exec cat /root/.urnetwork/billable_rate` on a container
// whose provider writes its state somewhere else entirely: no metric, "assume
// active traffic", full 30m timeout on a silent node.
func TestContainerStateDirHonoursContainerOverride(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want string
	}{
		{"override wins over HOME", "HOME=/root\nURNETWORK_STATE_DIR=/data/state\n", "/data/state"},
		{"HOME when there is no override", "HOME=/srv/urnet\nPATH=/usr/bin\n", "/srv/urnet/.urnetwork"},
		{"image default when neither is set", "PATH=/usr/bin\n", "/root/.urnetwork"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			shim := filepath.Join(t.TempDir(), "docker")
			script := "#!/bin/sh\ncat <<'ENVEOF'\n" + c.env + "ENVEOF\n"
			if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			setDockerTestBin(shim)
			t.Cleanup(func() { setDockerTestBin("") })

			if got := containerStateDir(dockerContainer{ID: "test-provider"}); got != c.want {
				t.Fatalf("containerStateDir = %q, want %q", got, c.want)
			}
		})
	}
}
