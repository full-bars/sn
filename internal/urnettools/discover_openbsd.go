package urnettools

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/user"
	"strings"
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
		managed := managedPID > 0 && row.pid == managedPID
		if managed {
			p.Supervisor = "openbsd-rcd"
			p.Binary = installedOpenBSDProvider()
		}
		if u, err := user.LookupId(row.uid); err == nil {
			p.User = u.Username
			p.StateDir = resolveDiscoveredStateDir(u.HomeDir, "")
			if p.StateDir != "" {
				p.StateHome = u.HomeDir
				h, err := openStateDirIn(p.StateHome, p.StateDir)
				if err != nil {
					p.IdentityRestricted = errors.Is(err, fs.ErrPermission)
					p.StateDir = ""
				} else {
					raw, err := h.readFile("jwt", maxJWTBytes)
					h.Close()
					p.IdentityRestricted = errors.Is(err, fs.ErrPermission)
					if err == nil {
						p.Network, p.NetworkID, p.JWTExpires, _ = decodeJWTFromBytes(raw)
					}
				}
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

func discoverStopped(running []Provider) []Provider {
	if openbsdServicePID() != 0 {
		return nil
	}
	for _, p := range running {
		if p.Supervisor == "openbsd-rcd" {
			return nil
		}
	}
	bin := installedOpenBSDProvider()
	if bin == "" || openbsdCheckScript(openbsdRcScriptPath("urnetwork"), bin) != nil {
		return nil
	}
	data, err := os.ReadFile(openbsdRcScriptPath("urnetwork"))
	if err != nil {
		return nil
	}
	var name string
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, `daemon_user="`); ok {
			name, _ = strings.CutSuffix(v, `"`)
			break
		}
	}
	if name == "" || name == "root" {
		return nil
	}
	for i, r := range name {
		if !(r >= 'a' && r <= 'z' || r == '_' || i > 0 && (r >= '0' && r <= '9' || r == '-')) {
			return nil
		}
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil
	}
	dir := resolveDiscoveredStateDir(u.HomeDir, "")
	if dir == "" || stateDirIsSymlink(dir) {
		return nil
	}
	p := Provider{User: u.Username, StateDir: dir, StateHome: u.HomeDir, Binary: bin, Supervisor: "openbsd-rcd"}
	h, err := openStateDirIn(p.StateHome, p.StateDir)
	if err == nil {
		var raw []byte
		raw, err = h.readFile("jwt", maxJWTBytes)
		h.Close()
		if err == nil {
			p.Network, p.NetworkID, p.JWTExpires, _ = decodeJWTFromBytes(raw)
		}
	}
	p.IdentityRestricted = errors.Is(err, fs.ErrPermission)
	return []Provider{p}
}

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
