package urnettools

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ps ucomm identifies a process by name, not its executable path.
func discoverProcesses() []Provider {
	out, err := execWithTimeout(context.Background(), 5*time.Second, "ps", "-A", "-o", "pid=,uid=,ucomm=")
	if err != nil {
		return nil
	}
	managedPID := openbsdServicePID()
	var providers []Provider
	for _, row := range parseOpenBSDProcesses(string(out)) {
		p := Provider{PID: row.pid, Running: true}
		if managedPID > 0 && row.pid == managedPID {
			p.Supervisor = "openbsd-rcd"
		}
		if u, err := user.LookupId(row.uid); err == nil {
			p.User = u.Username
			p.StateDir = resolveDiscoveredStateDir(u.HomeDir, "")
			if stateDirIsSymlink(p.StateDir) {
				p.StateDir = ""
			}
			if p.StateDir != "" {
				p.StateHome = u.HomeDir
				var jwtErr error
				p.Network, p.NetworkID, p.JWTExpires, jwtErr = decodeJWT(filepath.Join(p.StateDir, "jwt"))
				p.IdentityRestricted = errors.Is(jwtErr, fs.ErrPermission)
			}
		}
		providers = append(providers, p)
	}
	return providers
}

func init() {
	providerCommandFallback = func(p Provider) string {
		if p.User == "" || p.StateDir == "" {
			return ""
		}
		return installedOpenBSDProvider()
	}
}

var openbsdLibexecProviderPath = "/usr/local/libexec/urnetwork-provider/bin/urnetwork"
var openbsdProviderOwnerUID uint32 = 0

func installedOpenBSDProvider() string {
	path := openbsdLibexecProviderPath
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != openbsdProviderOwnerUID {
		return ""
	}
	perm := info.Mode().Perm()
	if perm&0o001 == 0 || perm&0o022 != 0 {
		return ""
	}
	if unix.Access(path, unix.X_OK) == nil {
		return path
	}
	return ""
}

func stateDirIsSymlink(dir string) bool {
	if dir == "" {
		return false
	}
	fi, err := os.Lstat(dir)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func discoverStopped(_ []Provider) []Provider { return nil }

func currentUserName() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

func narrowToAccessible(providers []Provider) []Provider {
	name := currentUserName()
	var out []Provider
	for _, p := range providers {
		if name != "" && p.User == name {
			out = append(out, p)
		}
	}
	return out
}

func platformIsPrivileged() bool { return os.Geteuid() == 0 }
