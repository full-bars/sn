//go:build unix

package urnettools

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// chownFdLikeStateOwner chowns an open fd to the owner of stateDir using
// fchown (fd-based, no path resolution — immune to symlink swap between
// close and chown). No-op when ownership already matches.
func chownFdLikeStateOwner(stateDir string, fd int) error {
	fi, err := os.Stat(stateDir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	var pst unix.Stat_t
	if err := unix.Fstat(fd, &pst); err != nil {
		return fmt.Errorf("fstat fd %d: %w", fd, err)
	}
	if pst.Uid == st.Uid && pst.Gid == st.Gid {
		return nil
	}
	return unix.Fchown(fd, int(st.Uid), int(st.Gid))
}
