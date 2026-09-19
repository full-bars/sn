//go:build unix

package urnettools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// stateDirHandle is an open directory descriptor for a provider state dir (or
// a subdirectory created under one). Every operation is relative to that
// descriptor (openat, mkdirat), so once the handle is open the directory
// cannot be swapped for a symlink or another tree between operations. The
// pathname APIs it replaces (write, then chown by path) re-resolve the state
// dir on every call, and the provider user owns that directory and can
// rename it at any time.
//
// Ownership handed to files and subdirectories is the owner recorded from
// fstat on the handle itself, so there is no second pathname lookup to
// redirect either.
//
// Scope of the guarantee: the handle pins the LEAF directory. Ancestors of
// path are resolved normally when the handle is opened, so a caller that got
// path from an untrusted source must have validated the whole path (see
// stateDirInsideHome) and a swap of an ancestor between that validation and
// openStateDirHandle is not caught here.
type stateDirHandle struct {
	fd       int
	path     string // for messages only; never used to access anything
	uid, gid uint32
}

// openStateDirHandle opens path as a directory without following a symlink
// in its final component. Ancestors are still resolved normally (legitimate
// system paths are often symlinks, e.g. macOS /var).
func openStateDirHandle(path string) (*stateDirHandle, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, fmt.Errorf("state dir %s is not a plain directory (symlink?): %w", path, err)
		}
		return nil, fmt.Errorf("open state dir %s: %w", path, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("fstat state dir %s: %w", path, err)
	}
	return &stateDirHandle{fd: fd, path: path, uid: st.Uid, gid: st.Gid}, nil
}

func (h *stateDirHandle) Close() error { return unix.Close(h.fd) }

// Path returns the pathname the handle was opened from, for messages and for
// showing the operator where files landed.
func (h *stateDirHandle) Path() string { return h.path }

// checkComponent rejects anything but a single plain path component, so a
// name can never walk out of the directory the handle pins.
func checkComponent(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return fmt.Errorf("invalid state file name %q", name)
	}
	return nil
}

// mkdirOwned creates the subdirectory name (mode 0700), fails if it already
// exists, hands it to the handle's owner through the open descriptor, and
// returns a handle on it.
func (h *stateDirHandle) mkdirOwned(name string) (*stateDirHandle, error) {
	if err := checkComponent(name); err != nil {
		return nil, err
	}
	if err := unix.Mkdirat(h.fd, name, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", filepath.Join(h.path, name), err)
	}
	// Reopen with O_NOFOLLOW: if the fresh directory was already swapped for
	// a symlink this fails instead of following it.
	fd, err := unix.Openat(h.fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Join(h.path, name), err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("fstat %s: %w", filepath.Join(h.path, name), err)
	}
	if st.Uid != h.uid || st.Gid != h.gid {
		if err := unix.Fchown(fd, int(h.uid), int(h.gid)); err != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("chown %s: %w", filepath.Join(h.path, name), err)
		}
	}
	return &stateDirHandle{fd: fd, path: filepath.Join(h.path, name), uid: h.uid, gid: h.gid}, nil
}

// writeOwned writes data to name inside the directory and hands the file to
// the handle's owner, all through descriptors: no symlink or FIFO is followed
// or written into, a hardlinked file is refused, and ownership is applied with
// fchown on the descriptor that was written.
func (h *stateDirHandle) writeOwned(name string, data []byte, perm os.FileMode) error {
	if err := checkComponent(name); err != nil {
		return err
	}
	full := filepath.Join(h.path, name)
	fd, err := unix.Openat(h.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, uint32(perm.Perm()))
	if err != nil {
		return fmt.Errorf("write %s: %v", full, err)
	}
	return writeThroughFd(fd, full, data, perm, func(fd int) error {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return err
		}
		if st.Uid == h.uid && st.Gid == h.gid {
			return nil
		}
		return unix.Fchown(fd, int(h.uid), int(h.gid))
	})
}

// readFile reads name from the directory without following a symlink or
// blocking on a FIFO, refusing more than max bytes.
func (h *stateDirHandle) readFile(name string, max int64) ([]byte, error) {
	if err := checkComponent(name); err != nil {
		return nil, err
	}
	full := filepath.Join(h.path, name)
	fd, err := unix.Openat(h.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f, err := finishStateRead(fd, full)
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

// rename renames oldName to newName inside the directory with renameat, so
// both names resolve against the pinned descriptor. Like rename(2) it
// replaces a symlink at newName instead of writing through it.
func (h *stateDirHandle) rename(oldName, newName string) error {
	if err := checkComponent(oldName); err != nil {
		return err
	}
	if err := checkComponent(newName); err != nil {
		return err
	}
	if err := unix.Renameat(h.fd, oldName, h.fd, newName); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", filepath.Join(h.path, oldName), newName, err)
	}
	return nil
}

// removeAll removes name and anything under it with unlinkat relative to
// descriptors, so no symlink is ever followed and a swapped ancestor cannot
// redirect the removal. A name that does not exist is not an error.
func (h *stateDirHandle) removeAll(name string) error {
	if err := checkComponent(name); err != nil {
		return err
	}
	if err := removeAllAt(h.fd, name); err != nil {
		return fmt.Errorf("remove %s: %w", filepath.Join(h.path, name), err)
	}
	return nil
}

func removeAllAt(dirfd int, name string) error {
	err := unix.Unlinkat(dirfd, name, 0)
	if err == nil || errors.Is(err, unix.ENOENT) {
		return nil
	}
	// Linux reports a directory as EISDIR, the BSDs and macOS as EPERM.
	if !errors.Is(err, unix.EISDIR) && !errors.Is(err, unix.EPERM) {
		return err
	}
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	d := os.NewFile(uintptr(fd), name)
	names, err := d.Readdirnames(-1)
	if err != nil {
		d.Close()
		return err
	}
	for _, n := range names {
		if err := removeAllAt(int(d.Fd()), n); err != nil {
			d.Close()
			return err
		}
	}
	if err := d.Close(); err != nil {
		return err
	}
	if err := unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

// adoptOwnerOf hands this directory to the owner recorded on ref, through the
// open descriptor, and makes it the owner applied to everything created
// under this handle afterwards. Use it for a directory the tool just created
// as root inside a provider's tree: its own fstat says root, which is not the
// owner the provider's unprivileged user needs.
func (h *stateDirHandle) adoptOwnerOf(ref *stateDirHandle) error {
	if h.uid == ref.uid && h.gid == ref.gid {
		return nil
	}
	if err := unix.Fchown(h.fd, int(ref.uid), int(ref.gid)); err != nil {
		return fmt.Errorf("chown %s: %w", h.path, err)
	}
	h.uid, h.gid = ref.uid, ref.gid
	return nil
}

// lockFile opens (creating if needed) the lock file name inside the
// directory and takes an exclusive flock on it, giving up after timeout
// (<= 0 blocks). The lock file is handed to the handle's owner so the
// unprivileged provider can open it for its own merge; a chown failure is
// reported on stderr rather than failing the lock.
func (h *stateDirHandle) lockFile(name string, timeout time.Duration) (func(), error) {
	if err := checkComponent(name); err != nil {
		return nil, err
	}
	full := filepath.Join(h.path, name)
	fd, err := unix.Openat(h.fd, name, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", full, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("fstat lock file %s: %w", full, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink > 1 {
		unix.Close(fd)
		return nil, fmt.Errorf("refusing lock file %s: not a regular file with a single link", full)
	}
	if st.Uid != h.uid || st.Gid != h.gid {
		if err := unix.Fchown(fd, int(h.uid), int(h.gid)); err != nil {
			fmt.Fprintf(os.Stderr, "warn: could not hand %s to the state dir owner: %v (the provider may be unable to read it)\n", full, err)
		}
	}
	f := os.NewFile(uintptr(fd), full)
	if err := flockWithTimeout(int(f.Fd()), timeout); err != nil {
		f.Close()
		return nil, fmt.Errorf("flock %s: %w", full, err)
	}
	return func() {
		unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}
