//go:build windows

package urnettools

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// stateDirHandle on Windows is a path wrapper. There is no openat/fchown to
// pin the directory, so each operation goes through the pathname-based
// primitives (writeStateFile, openStateFileNoFollow), which carry their own
// Lstat/SameFile guards. The directory-swap hardening the unix handle
// provides is not available here; see the unix implementation.
type stateDirHandle struct {
	path string
}

func openStateDirHandle(path string) (*stateDirHandle, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("open state dir %s: %w", path, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("state dir %s is not a plain directory", path)
	}
	return &stateDirHandle{path: path}, nil
}

func (h *stateDirHandle) Close() error { return nil }

func (h *stateDirHandle) Path() string { return h.path }

func checkComponent(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid state file name %q", name)
	}
	return nil
}

func (h *stateDirHandle) mkdirOwned(name string) (*stateDirHandle, error) {
	if err := checkComponent(name); err != nil {
		return nil, err
	}
	full := filepath.Join(h.path, name)
	if err := os.Mkdir(full, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", full, err)
	}
	return &stateDirHandle{path: full}, nil
}

func (h *stateDirHandle) writeOwned(name string, data []byte, perm os.FileMode) error {
	if err := checkComponent(name); err != nil {
		return err
	}
	return writeStateFile(h.path, name, data, perm)
}

func (h *stateDirHandle) readFile(name string, max int64) ([]byte, error) {
	if err := checkComponent(name); err != nil {
		return nil, err
	}
	f, err := openStateFileNoFollow(h.path, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s exceeds the %d byte state file limit", name, max)
	}
	return b, nil
}

func (h *stateDirHandle) rename(oldName, newName string) error {
	if err := checkComponent(oldName); err != nil {
		return err
	}
	if err := checkComponent(newName); err != nil {
		return err
	}
	return os.Rename(filepath.Join(h.path, oldName), filepath.Join(h.path, newName))
}

func (h *stateDirHandle) removeAll(name string) error {
	if err := checkComponent(name); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(h.path, name))
}

// adoptOwnerOf is a no-op: there is no Unix ownership to hand over.
func (h *stateDirHandle) adoptOwnerOf(ref *stateDirHandle) error { return nil }

// lockFile takes the exclusive lock on name; the timeout is not applied on
// Windows, where LockFileEx blocks.
func (h *stateDirHandle) lockFile(name string, timeout time.Duration) (func(), error) {
	if err := checkComponent(name); err != nil {
		return nil, err
	}
	return acquireExclusiveLock(filepath.Join(h.path, name))
}
