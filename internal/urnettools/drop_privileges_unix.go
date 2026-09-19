//go:build unix

package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

// dropPrivilegesTo runs cmd as username when the tool is root and the target
// differs from the caller (cross-user deployment: a root-run urnet-tools must
// not write the provider's jwt/network as root, or the provider cannot read it).
// No-op when already the target user or when the tool is not root.
//
// FAILURE IS AN ERROR, never a silent root fallback: a root-run tool that
// cannot resolve the target user must not go on to execute the provider
// binary as root — C1 (a discovery User whose lookup fails, e.g. an
// attacker-supplied USER= value, would otherwise run an arbitrary binary
// with full privileges and HOME=/root). The caller decides whether to
// proceed unprivileged (e.g. only as the operator's own uid).
func dropPrivilegesTo(username string, cmd *exec.Cmd) error {
	if username == "" {
		return nil
	}
	if !isRootImpl() {
		return nil
	}
	target, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("resolve user %q: %w", username, err)
	}
	uid, err := strconv.Atoi(target.Uid)
	if err != nil {
		return fmt.Errorf("parse uid for %q: %w", username, err)
	}
	gid, err := strconv.Atoi(target.Gid)
	if err != nil {
		return fmt.Errorf("parse gid for %q: %w", username, err)
	}
	if uint32(os.Geteuid()) == uint32(uid) {
		return nil // already running as the target user
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
	}
	return nil
}

// isRootImpl reports whether the tool runs as uid 0.
func isRootImpl() bool {
	return os.Geteuid() == 0
}
