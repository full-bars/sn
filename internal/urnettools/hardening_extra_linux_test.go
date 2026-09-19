//go:build linux

package urnettools

import "testing"

// A host unit whose NAME merely contains a runtime word is not a container;
// real runtime scopes/slices are.
func TestClassifyContainerCgroupNearMisses(t *testing.T) {
	cases := []struct {
		cg   string
		want bool
	}{
		{"0::/system.slice/urnetwork-docker-helper.service", false},
		{"0::/system.slice/lxcfs.service", false},
		{"0::/system.slice/containerd.service", false},
		{"0::/system.slice/docker.service", false},
		{"0::/system.slice/my-libpod-thing.service", false},
		{"0::/system.slice/kubepods-monitor.service", false},
		{"0::/system.slice/docker-abc123.scope", true},
		{"5:cpu:/docker/abc123456789", true},
		{"0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podx.slice/cri-containerd-abc.scope", true},
		{"0::/kubepods/burstable/pod123/abc123", true},
		{"0::/user.slice/user-1000.slice/user@1000.service/user.slice/libpod-abc123.scope", true},
		{"7:devices:/lxc.payload.abc123/container", true},
		{"0::/machine.slice/machine-foo.scope", true},
		{"12:pids:/user.slice\n0::/system.slice/docker-abc.scope\n", true},
	}
	for _, c := range cases {
		if got := classifyContainerByNamespaceAndCgroup(true, c.cg); got != c.want {
			t.Errorf("classify(%q) = %v, want %v", c.cg, got, c.want)
		}
	}
}
