//go:build unix

package urnettools

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// writeStateFile writes data to a file inside stateDir with O_NOFOLLOW to
// prevent symlink-following attacks. If a symlink exists at the target path,
// the write is refused rather than silently overwriting the symlink target.
// This prevents a privileged-provider-user from planting
// node_name -> /etc/shadow and having root overwrite shadow via urnet-tools set.
func writeStateFile(stateDir, name string, data []byte, perm os.FileMode) error {
	path := filepath.Join(stateDir, name)

	// Reject if path is already a symlink (attack indicator)
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to write %s: path is a symlink (possible symlink attack)", path)
	}

	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW, uint32(perm))
	if err != nil {
		return fmt.Errorf("write %s: %v", path, err)
	}
	// The *os.File owns fd from here, and f.Close is its only close.
	// Closing fd directly as well double-closed it: the File's finalizer
	// later closed whatever descriptor had reused the number.
	f := os.NewFile(uintptr(fd), path)

	// os.File.Write loops on short writes. Chmod enforces perm on existing
	// files, where O_CREAT's mode argument is ignored.
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %v", path, err)
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return fmt.Errorf("chmod %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %v", path, err)
	}
	return nil
}

// chownStateFile changes ownership of a file without following symlinks.
// Uses Lchown instead of Chown so a symlink at the target path is not followed.
func chownStateFile(path string, uid, gid int) error {
	return unix.Lchown(path, uid, gid)
}

// chownStateDir changes ownership of the state directory without following
// symlinks. Uses Lchown instead of os.Chown.
func chownStateDir(path string, uid, gid int) error {
	return unix.Lchown(path, uid, gid)
}
