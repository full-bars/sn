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

	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW|unix.O_NONBLOCK, uint32(perm))
	if err != nil {
		return fmt.Errorf("write %s: %v", path, err)
	}
	// O_NONBLOCK was only for FIFO detection — the actual file (regular,
	// freshly created or truncated) must be written in blocking mode, or a
	// slow disk could EAGAIN mid-write. Clear it on the open fd.
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFL, unix.O_WRONLY); err != nil {
		unix.Close(fd)
		return fmt.Errorf("clear nonblock on %s: %v", path, err)
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

// openStateFileNoFollow opens a file in stateDir with O_NOFOLLOW so a
// planted symlink cannot redirect reads or writes. Returns the open *os.File;
// callers close it. Symlink targets, directories, and permission failures all
// error. Kept next to writeStateFile because both are unix-only fd-level
// state access primitives.
func openStateFileNoFollow(stateDir, name string) (*os.File, error) {
	full := filepath.Join(stateDir, name)
	fd, err := unix.Open(full, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("fstat %s: %w", full, err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return nil, fmt.Errorf("refusing to read %s: not a regular file (symlink or directory)", full)
	}
	return os.NewFile(uintptr(fd), full), nil
}
