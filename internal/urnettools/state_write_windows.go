//go:build windows

package urnettools

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeStateFile writes data to a file. On Windows, symlink attacks via
// os.WriteFile are less exploitable (no setuid/chown escalation model),
// so this is a straightforward wrapper.
func writeStateFile(stateDir, name string, data []byte, perm os.FileMode) error {
	path := filepath.Join(stateDir, name)
	return os.WriteFile(path, data, perm)
}

// chownStateFile is a no-op on Windows (no Unix ownership model).
func chownStateFile(path string, uid, gid int) error {
	return nil
}

// chownStateDir is a no-op on Windows (no Unix ownership model).
func chownStateDir(path string, uid, gid int) error {
	return nil
}

// openStateFileNoFollow opens a state file without following symlinks. On
// Windows, symlink attacks are less exploitable (no setuid/chown escalation),
// so this delegates to os.Open.
func openStateFileNoFollow(stateDir, name string) (*os.File, error) {
	p := filepath.Join(stateDir, name)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", p, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing to read %s: path is a symlink", p)
	}
	return os.Open(p)
}
