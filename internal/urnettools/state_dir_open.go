package urnettools

import (
	"os"
	"path/filepath"
	"strings"
)

// openStateDirIn opens dir as a state-dir handle. When root (a trusted home,
// see Provider.StateHome) is set and dir lies strictly beneath it, the path
// is walked from root without following symlinks (openStateDirWithin), so no
// intermediate component can be swapped after the path was validated.
// Otherwise only the leaf is pinned (openStateDirHandle): a dir outside the
// trusted home is operator- or root-controlled, not provider-controlled.
func openStateDirIn(root, dir string) (*stateDirHandle, error) {
	if root != "" {
		cleanRoot := filepath.Clean(root)
		if strings.HasPrefix(filepath.Clean(dir), cleanRoot+string(filepath.Separator)) {
			return openStateDirWithin(root, dir, false)
		}
	}
	return openStateDirHandle(dir)
}

// openProviderStateDir opens p's state dir with the strongest pinning its
// discovery provenance allows.
func openProviderStateDir(p Provider) (*stateDirHandle, error) {
	return openStateDirIn(p.StateHome, p.StateDir)
}

// openStateDirInCreate is openStateDirIn that also creates dir (mode 0700)
// when it does not exist. Beneath a trusted root every missing component is
// made with mkdirat and handed to the root's owner; otherwise it falls back
// to MkdirAll on the path followed by a leaf-pinned open.
func openStateDirInCreate(root, dir string) (*stateDirHandle, error) {
	if root != "" {
		cleanRoot := filepath.Clean(root)
		if strings.HasPrefix(filepath.Clean(dir), cleanRoot+string(filepath.Separator)) {
			return openStateDirWithin(root, dir, true)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return openStateDirHandle(dir)
}
