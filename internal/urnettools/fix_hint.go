package urnettools

// fix_hint.go turns a lineage mismatch into something the operator can act on.
//
// The tool deliberately does not replace itself. A self-swapping binary on
// unattended hosts is the one failure mode that can leave a box with no tool
// and no state path back, and the fix it would perform is a two-line download
// the operator (or their deploy pass) already knows how to run.

import (
	"fmt"
	"runtime"
	"strings"
)

// releaseRepoForProvider maps a provider's module path to the repo that
// publishes the tool built for it.
//
// The 3.23-fix lineage and meso-miner share the module path
// github.com/urnetwork/connect, so their version shape decides between them:
// only meso-miner stamps a -meso suffix.
func releaseRepoForProvider(modulePath, version string) (string, bool) {
	switch modulePath {
	case "github.com/urfoundation/sn":
		return "full-bars/sn", true
	case "github.com/urnetwork/connect":
		if strings.HasSuffix(version, "-meso") {
			return "full-bars/meso-miner", true
		}
		return "full-bars/urnetwork-3.23-fix", true
	}
	return "", false
}

// toolInstallHint returns the copy-pasteable command that installs the tool
// published alongside a given provider version. self is the running tool's
// absolute path, so the replacement lands where the shell already finds it.
//
// The download lands on a staged name and is moved into place only after curl
// succeeds: `curl -o <self>` writes as it reads, so a transfer that dies
// halfway replaces a working tool with a truncated one.
func toolInstallHint(repo, version, self, goos, goarch string) string {
	asset := "urnet-tools-" + goos + "-" + goarch
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, version, asset)
	staged := self + ".new"
	if goos == "windows" {
		// A running .exe cannot be replaced on Windows, so the hint stages the
		// download under a plain .new name and says plainly when the move
		// happens. The staged file needs no .exe suffix: it is moved, not run.
		return fmt.Sprintf("       curl -fL -o %s %s\n       then, once this tool has exited: move /Y %s %s", staged, url, staged, self)
	}
	return fmt.Sprintf("       curl -fL -o %s %s && chmod +x %s && mv %s %s", staged, url, staged, staged, self)
}

// hostPlatform reports the current GOOS/GOARCH pair, in one place so tests can
// reason about the asset name without duplicating the runtime import.
func hostPlatform() (string, string) {
	return runtime.GOOS, runtime.GOARCH
}
