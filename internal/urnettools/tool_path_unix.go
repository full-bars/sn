//go:build unix

package urnettools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// toolLinkNames are the binaries linked next to each other in the install dir.
var toolLinkNames = []string{"urnet-tools", "urnetwork"}

// linkToolsIntoDir symlinks each tool that exists in srcDir into dir, creating
// dir. A real file that is not a symlink is left alone (it is not ours); a
// symlink that already points at the right place is left alone; a stale one
// is repointed. It returns the names it created or repointed.
func linkToolsIntoDir(dir, srcDir string) (changed []string, err error) {
	if dir == srcDir {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	for _, name := range toolLinkNames {
		src := filepath.Join(srcDir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(dir, name)
		if fi, err := os.Lstat(dst); err == nil {
			if fi.Mode()&os.ModeSymlink == 0 {
				continue
			}
			if cur, err := os.Readlink(dst); err == nil && cur == src {
				continue
			}
			if err := os.Remove(dst); err != nil {
				return changed, err
			}
		}
		if err := os.Symlink(src, dst); err != nil {
			return changed, err
		}
		changed = append(changed, name)
	}
	return changed, nil
}

// sudoLinkTools does the same into a root-owned dir through passwordless sudo.
func sudoLinkTools(dir, srcDir string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, "sudo", "-n", "true").Run() != nil {
		return nil
	}
	if exec.CommandContext(ctx, "sudo", "-n", "mkdir", "-p", dir).Run() != nil {
		return nil
	}
	var changed []string
	for _, name := range toolLinkNames {
		src := filepath.Join(srcDir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(dir, name)
		if fi, err := os.Lstat(dst); err == nil {
			if fi.Mode()&os.ModeSymlink == 0 {
				continue // a real file, not ours
			}
			if cur, err := os.Readlink(dst); err == nil && cur == src {
				continue
			}
		}
		if exec.CommandContext(ctx, "sudo", "-n", "ln", "-sfn", src, dst).Run() == nil {
			changed = append(changed, name)
		}
	}
	return changed
}

// ensureToolOnPath makes the running urnet-tools reachable from the shells the
// installer's ~/.bashrc export never reaches: the non-interactive shell behind
// `ssh host urnet-tools ...` and cron (via ~/.local/bin), and root or any other
// user (via /usr/local/bin). Nodes installed before the installer did this
// only ever run `update`, never the installer again, so update repairs them.
// Best-effort and quiet unless it changes something.
func ensureToolOnPath() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	if filepath.Base(exe) != "urnet-tools" {
		return // not an installed tool binary (for example a test binary)
	}
	srcDir := filepath.Dir(exe)

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dir := filepath.Join(home, ".local", "bin")
		if changed, err := linkToolsIntoDir(dir, srcDir); err != nil {
			fmt.Fprintf(os.Stderr, "note: could not link urnet-tools into %s: %v\n", dir, err)
		} else if len(changed) > 0 {
			fmt.Printf("linked %v into %s so shells without ~/.bashrc find them\n", changed, dir)
		}
	}
	const sysDir = "/usr/local/bin"
	if os.Geteuid() == 0 {
		if changed, err := linkToolsIntoDir(sysDir, srcDir); err != nil {
			fmt.Fprintf(os.Stderr, "note: could not link urnet-tools into %s: %v\n", sysDir, err)
		} else if len(changed) > 0 {
			fmt.Printf("linked %v into %s\n", changed, sysDir)
		}
	} else if changed := sudoLinkTools(sysDir, srcDir); len(changed) > 0 {
		fmt.Printf("linked %v into %s so root and other users find them\n", changed, sysDir)
	}
}
