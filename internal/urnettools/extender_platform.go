package urnettools

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PlatformConfigurer defines the operations needed to configure and verify extender requirements.
type PlatformConfigurer interface {
	Platform() string
	CheckRequirement(binary, user string) (configured bool, desc string, err error)
	ApplyRequirement(binary, user string) (applied bool, desc string, err error)
	VerifyBind(binary, user string) (ok bool, err error)
}

// platformPlanner is an optional interface implemented by PlatformConfigurers
// that can report the exact commands ApplyRequirement will run.
type platformPlanner interface {
	PlanRequirement(binary, user string) ([]setupCommand, error)
}

// SystemPlatformConfigurer is the default implementation that talks to OS binaries and kernel APIs.
type SystemPlatformConfigurer struct {
	PlatformName string
	ExecCmd      func(name string, args ...string) ([]byte, error)
	ReadFile     func(name string) ([]byte, error)
	ReadDir      func(name string) ([]os.DirEntry, error)
	WriteFile    func(name string, data []byte, perm os.FileMode) error
	ListenTCP    func(network, address string) (net.Listener, error)
	LookupPath   func(file string) (string, error)
	IsRoot       func() bool
	// Lstat inspects the provider binary without following a symlink. Nil
	// skips the check, for test doubles that name no real file.
	Lstat func(name string) (os.FileInfo, error)
	// Route selects how an unprivileged provider gets TCP 443 on Linux:
	// "sysctl", "capability" or "skip". Empty keeps the file-capability
	// check, with a live low ip_unprivileged_port_start also accepted.
	Route string
	// SysctlUndo is the recovery command printed with the plan. Setup fills
	// it before any write so the text matches the state it saw first.
	SysctlUndo string
	// SysctlDirs and SysctlConf override the sysctl search path for tests.
	// Nil or empty means the documented default.
	SysctlDirs []string
	SysctlConf string
}

// NewSystemPlatformConfigurer returns a SystemPlatformConfigurer for the platform.
func NewSystemPlatformConfigurer(platform string) *SystemPlatformConfigurer {
	return &SystemPlatformConfigurer{
		PlatformName: platform,
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			if name == "sudo" && len(args) > 0 && args[len(args)-1] == "--extender-bind-probe" {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				return exec.CommandContext(ctx, name, args...).CombinedOutput()
			}
			cmd := exec.Command(name, args...)
			return cmd.CombinedOutput()
		},
		ReadFile:   os.ReadFile,
		ReadDir:    os.ReadDir,
		WriteFile:  os.WriteFile,
		ListenTCP:  net.Listen,
		LookupPath: exec.LookPath,
		Lstat:      os.Lstat,
		IsRoot: func() bool {
			return os.Geteuid() == 0
		},
	}
}

func (sp *SystemPlatformConfigurer) Platform() string {
	return sp.PlatformName
}

// CheckRequirement checks whether the host platform requirement is already satisfied.
func (sp *SystemPlatformConfigurer) CheckRequirement(binary, user string) (bool, string, error) {
	switch sp.PlatformName {
	case "linux":
		// A route is only chosen when some provider is unprivileged; a root
		// provider on the same host needs none of it.
		if user == "root" && sp.Route != "" {
			return true, "provider running as root (no file capability needed)", nil
		}
		switch sp.Route {
		case "sysctl":
			return sp.checkLinuxSysctl()
		case "capability":
			return sp.checkLinuxCapability(binary)
		}
		return sp.checkLinux(binary)
	case "freebsd":
		return sp.checkFreeBSD(user)
	case "windows":
		return sp.checkWindows()
	case "darwin":
		return sp.checkDarwin()
	default:
		return false, fmt.Sprintf("unsupported platform %s", sp.PlatformName), nil
	}
}

// PlanRequirement returns the exact commands that ApplyRequirement will run.
func (sp *SystemPlatformConfigurer) PlanRequirement(binary, user string) ([]setupCommand, error) {
	switch sp.PlatformName {
	case "linux":
		if user == "root" && sp.Route != "" {
			return nil, nil
		}
		switch sp.Route {
		case "sysctl":
			return sp.planLinuxSysctl()
		case "skip":
			return nil, nil
		}
		if binary == "" {
			return nil, fmt.Errorf("cannot apply cap_net_bind_service: no provider binary path known")
		}
		if sp.Lstat != nil {
			info, err := sp.Lstat(binary)
			if err != nil {
				return nil, fmt.Errorf("cannot apply cap_net_bind_service: %w", err)
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("refusing to set cap_net_bind_service on %s: it is %s, not a regular file; point setup at the real provider binary", binary, info.Mode().Type())
			}
		}
		if sp.ReadFile != nil {
			if status, err := sp.ReadFile("/proc/self/status"); err == nil && bindCapabilityBlocked(status) {
				return nil, fmt.Errorf("cap_net_bind_service is excluded from the process bounding set")
			}
		}
		args, err := sp.linuxCapabilityArgs(binary)
		if err != nil {
			return nil, err
		}
		return []setupCommand{{name: "setcap", args: args}}, nil
	case "freebsd":
		return nil, nil
	case "windows":
		return []setupCommand{
			{name: "netsh", args: []string{"advfirewall", "firewall", "add", "rule", "name=URnetwork Extender", "dir=in", "action=allow", "protocol=TCP", "localport=443"}},
			{name: "netsh", args: []string{"advfirewall", "firewall", "add", "rule", "name=URnetwork Extender UDP", "dir=in", "action=allow", "protocol=UDP", "localport=443,4053"}},
		}, nil
	case "darwin":
		_, _, err := sp.checkDarwin()
		return nil, err
	default:
		return nil, fmt.Errorf("unsupported platform %s", sp.PlatformName)
	}
}

// ApplyRequirement applies the privileged platform requirement.
func (sp *SystemPlatformConfigurer) ApplyRequirement(binary, user string) (bool, string, error) {
	switch sp.PlatformName {
	case "linux":
		if user == "root" && sp.Route != "" {
			return false, "provider running as root (no file capability needed)", nil
		}
		switch sp.Route {
		case "sysctl":
			return sp.applyLinuxSysctl()
		case "skip":
			return false, "port 443 binding setup skipped", nil
		}
		return sp.applyLinux(binary)
	case "freebsd":
		return sp.applyFreeBSD(user)
	case "windows":
		return sp.applyWindows()
	case "darwin":
		return sp.applyDarwin()
	default:
		return false, "", fmt.Errorf("unsupported platform %s", sp.PlatformName)
	}
}

// VerifyBind runs the provider's probe as its user; false without error is unverified.
func (sp *SystemPlatformConfigurer) VerifyBind(binary, user string) (bool, error) {
	if sp.PlatformName != "linux" || binary == "" || user == "" || sp.LookupPath == nil || sp.ExecCmd == nil {
		return false, nil
	}
	if _, err := sp.LookupPath("sudo"); err != nil {
		return false, nil
	}
	out, err := sp.ExecCmd("sudo", "-n", "-u", user, "--", binary, "--extender-bind-probe")
	text := strings.TrimSpace(string(out))
	if err == nil && text == "extender-bind-probe: proven" {
		return true, nil
	}
	if strings.HasPrefix(text, "extender-bind-probe: failed:") {
		return false, fmt.Errorf("%s", text)
	}
	return false, nil
}

// unitNoNewPrivileges is implemented by configurers that can ask systemd
// whether a unit sets NoNewPrivileges. It is optional so test doubles of
// PlatformConfigurer need not grow a method.
type unitNoNewPrivileges interface {
	UnitNoNewPrivileges(unit string) (bool, error)
}

// UnitNoNewPrivileges reports whether the systemd unit has NoNewPrivileges
// set. With it, the kernel ignores file capabilities on exec, so the
// cap_net_bind_service that setup grants does not reach the service.
func (sp *SystemPlatformConfigurer) UnitNoNewPrivileges(unit string) (bool, error) {
	if sp.PlatformName != "linux" || unit == "" || sp.ExecCmd == nil {
		return false, nil
	}
	out, err := sp.ExecCmd("systemctl", "show", "-p", "NoNewPrivileges", "--value", unit)
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(string(out)) {
	case "yes", "true", "1", "on":
		return true, nil
	}
	return false, nil
}

// --- Linux implementation ---

func (sp *SystemPlatformConfigurer) checkLinux(binary string) (bool, string, error) {
	if v, ok := sp.liveUnprivilegedPortStart(); ok && v <= 443 {
		if v == 443 {
			return true, "TCP 443 is available without a file capability (ip_unprivileged_port_start=443)", nil
		}
		return true, fmt.Sprintf("TCP 443 is available without a file capability (ip_unprivileged_port_start=%d is already lower than 443)", v), nil
	}
	return sp.checkLinuxCapability(binary)
}

func (sp *SystemPlatformConfigurer) checkLinuxCapability(binary string) (bool, string, error) {
	if binary == "" {
		return false, "no provider binary path known", nil
	}
	out, err := sp.ExecCmd("getcap", binary)
	if err != nil {
		return false, fmt.Sprintf("getcap %s failed: %v", binary, err), nil
	}
	capOut := string(out)
	if linuxHasBindCapability(binary, capOut) {
		return true, fmt.Sprintf("cap_net_bind_service=+ep already set on %s", binary), nil
	}
	return false, fmt.Sprintf("cap_net_bind_service=+ep missing on %s", binary), nil
}

// linuxHasBindCapability parses the capability clauses, not substrings of names.
func linuxHasBindCapability(binary, output string) bool {
	if !strings.HasPrefix(output, binary+" ") {
		return false
	}
	for _, clause := range strings.Fields(strings.TrimPrefix(output, binary)) {
		i := strings.IndexAny(clause, "=+")
		if i < 0 {
			continue
		}
		flags := clause[i+1:]
		if !strings.Contains(flags, "e") || !strings.Contains(flags, "p") {
			continue
		}
		for _, name := range strings.Split(clause[:i], ",") {
			if name == "cap_net_bind_service" {
				return true
			}
		}
	}
	return false
}

var linuxCapabilityClause = regexp.MustCompile(`^(?:cap_[a-z0-9_]+(?:,cap_[a-z0-9_]+)*[=+][eip]*|=)$`)

// linuxCapabilityArgs reads the whole file capability set, including its namespace owner.
func (sp *SystemPlatformConfigurer) linuxCapabilityArgs(binary string) ([]string, error) {
	out, err := sp.ExecCmd("getcap", "-n", binary)
	if err != nil {
		return nil, fmt.Errorf("cannot read existing file capabilities for %s: %w", binary, err)
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return []string{"cap_net_bind_service=+ep", binary}, nil
	}
	if !strings.HasPrefix(text, binary+" ") {
		return nil, fmt.Errorf("cannot parse existing file capabilities for %s", binary)
	}
	fields := strings.Fields(strings.TrimPrefix(text, binary))
	var args []string
	if len(fields) > 0 && strings.HasPrefix(fields[len(fields)-1], "[rootid=") {
		root := strings.TrimSuffix(strings.TrimPrefix(fields[len(fields)-1], "[rootid="), "]")
		if !strings.HasSuffix(fields[len(fields)-1], "]") {
			return nil, fmt.Errorf("cannot parse capability namespace owner")
		}
		id, err := strconv.ParseUint(root, 10, 32)
		if err != nil || id == 0 {
			return nil, fmt.Errorf("cannot parse capability namespace owner")
		}
		args = append(args, "-n", root)
		fields = fields[:len(fields)-1]
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("cannot parse existing file capabilities for %s", binary)
	}
	for _, field := range fields {
		if !linuxCapabilityClause.MatchString(field) {
			return nil, fmt.Errorf("cannot parse existing file capabilities for %s", binary)
		}
	}
	return append(args, strings.Join(fields, " ")+" cap_net_bind_service+ep", binary), nil
}

func (sp *SystemPlatformConfigurer) applyLinux(binary string) (bool, string, error) {
	if binary == "" {
		return false, "", fmt.Errorf("cannot apply cap_net_bind_service: no provider binary path known")
	}
	// setcap runs as root on a path the provider user may own (an install
	// under their home is a supported location). setcap follows a symlink, so a
	// link swapped in for the binary would put the capability on whatever it
	// points at. Refuse anything that is not the regular file itself.
	if sp.Lstat != nil {
		info, err := sp.Lstat(binary)
		if err != nil {
			return false, "", fmt.Errorf("cannot apply cap_net_bind_service: %w", err)
		}
		if !info.Mode().IsRegular() {
			return false, "", fmt.Errorf("refusing to set cap_net_bind_service on %s: it is %s, not a regular file; point setup at the real provider binary", binary, info.Mode().Type())
		}
	}
	// Applying a capability outside the bounding set makes exec fail.
	if sp.ReadFile != nil {
		if status, err := sp.ReadFile("/proc/self/status"); err == nil && bindCapabilityBlocked(status) {
			return false, "", fmt.Errorf("cap_net_bind_service is excluded from the process bounding set")
		}
	}
	// Re-read the complete current set before applying; setcap replaces it.
	args, err := sp.linuxCapabilityArgs(binary)
	if err != nil {
		return false, "", err
	}
	if out, err := sp.ExecCmd("setcap", args...); err != nil {
		return false, "", fmt.Errorf("setcap %s failed: %s: %w", binary, strings.TrimSpace(string(out)), err)
	}

	// Verify re-reading getcap
	out, err := sp.ExecCmd("getcap", binary)
	if err != nil {
		return false, "", fmt.Errorf("re-reading getcap %s failed: %w", binary, err)
	}
	capOut := string(out)
	if !linuxHasBindCapability(binary, capOut) {
		return false, "", fmt.Errorf("re-reading getcap %s: capability cap_net_bind_service=+ep not present (got %q)", binary, strings.TrimSpace(capOut))
	}

	return true, fmt.Sprintf("applied cap_net_bind_service=+ep on %s (verified with getcap)", binary), nil
}

// --- Linux sysctl route ---

const (
	extenderSysctlPortStartPath = "/proc/sys/net/ipv4/ip_unprivileged_port_start"
	extenderSysctlKey           = "net.ipv4.ip_unprivileged_port_start"
	extenderSysctlDropin        = "/etc/sysctl.d/99-urnetwork-extender.conf"
	// The backup name has no .conf suffix on purpose: sysctl.d only loads
	// *.conf files, so the saved copy never takes effect.
	extenderSysctlBackup  = extenderSysctlDropin + ".pre-urnetwork"
	extenderSysctlContent = extenderSysctlKey + "=443\n"
)

// extenderSysctlDirs is the systemd sysctl.d(5) search path, highest
// precedence first. A file name in an earlier directory masks the same name in
// a later one.
var extenderSysctlDirs = []string{"/etc/sysctl.d", "/run/sysctl.d", "/usr/local/lib/sysctl.d", "/usr/lib/sysctl.d", "/lib/sysctl.d"}

const extenderSysctlConf = "/etc/sysctl.conf"

// liveUnprivilegedPortStart reads the running kernel value.
func (sp *SystemPlatformConfigurer) liveUnprivilegedPortStart() (int, bool) {
	if sp.ReadFile == nil {
		return 0, false
	}
	b, err := sp.ReadFile(extenderSysctlPortStartPath)
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || v < 0 {
		return 0, false
	}
	return v, true
}

func (sp *SystemPlatformConfigurer) checkLinuxSysctl() (bool, string, error) {
	v, ok := sp.liveUnprivilegedPortStart()
	if !ok || v > 443 {
		return false, "sysctl route not active", nil
	}
	if !sp.sysctlPersisted() {
		return false, fmt.Sprintf("ip_unprivileged_port_start=%d live, but not persisted; setup can write %s", v, extenderSysctlDropin), nil
	}
	if v == 443 {
		return true, "TCP 443 is available without a file capability (ip_unprivileged_port_start=443, persisted)", nil
	}
	return true, fmt.Sprintf("TCP 443 is available without a file capability (ip_unprivileged_port_start=%d is already lower than 443, persisted)", v), nil
}

// sysctlDropinExists refuses unreadable files and symlinks before an overwrite.
func (sp *SystemPlatformConfigurer) sysctlDropinExists() (bool, error) {
	if sp.Lstat != nil {
		info, err := sp.Lstat(extenderSysctlDropin)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspecting %s: %w", extenderSysctlDropin, err)
		}
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("refusing to replace non-regular sysctl drop-in %s", extenderSysctlDropin)
		}
		return true, nil
	}
	if sp.ReadFile == nil {
		return false, fmt.Errorf("cannot inspect sysctl drop-in")
	}
	_, err := sp.ReadFile(extenderSysctlDropin)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// sysctlRecovery is the command that undoes the route: restore the saved
// drop-in or remove ours, then put the prior live value back. Setup takes it
// before any write so it describes the state setup found.
func (sp *SystemPlatformConfigurer) sysctlRecovery() (string, error) {
	exists, err := sp.sysctlDropinExists()
	if err != nil {
		return "", err
	}
	command := "sudo rm " + extenderSysctlDropin
	if exists {
		command = "sudo mv " + extenderSysctlBackup + " " + extenderSysctlDropin
	}
	if v, ok := sp.liveUnprivilegedPortStart(); ok && v <= 65535 {
		return fmt.Sprintf("%s && sudo sysctl -w %s=%d", command, extenderSysctlKey, v), nil
	}
	return command + "\nAfter undo, check: sysctl " + extenderSysctlKey + ". Prior live value unknown; set the value your distribution uses (usually 1024).", nil
}

// sysctlFiles lists the files systemd-sysctl applies, in application order:
// the merged *.conf set sorted by name (same-named files in earlier
// directories mask later ones), then /etc/sysctl.conf. The last assignment
// applied wins. A directory that cannot be listed (other than a missing one)
// makes the answer unknown.
func (sp *SystemPlatformConfigurer) sysctlFiles() ([]string, bool) {
	dirs, conf := sp.SysctlDirs, sp.SysctlConf
	if len(dirs) == 0 {
		dirs = extenderSysctlDirs
	}
	if conf == "" {
		conf = extenderSysctlConf
	}
	var paths []string
	if sp.ReadDir == nil {
		paths = append(paths, extenderSysctlDropin)
	} else {
		byName := map[string]string{}
		for _, dir := range dirs {
			entries, err := sp.ReadDir(dir)
			if err != nil && !os.IsNotExist(err) {
				return nil, false
			}
			for _, entry := range entries {
				name := entry.Name()
				if !strings.HasSuffix(name, ".conf") || entry.IsDir() {
					continue
				}
				if _, masked := byName[name]; !masked {
					byName[name] = filepath.Join(dir, name)
				}
			}
		}
		names := make([]string, 0, len(byName))
		for name := range byName {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			paths = append(paths, byName[name])
		}
	}
	return append(paths, conf), true
}

// sysctlPersisted reports whether the boot-time configuration keeps the low
// port threshold. A live value alone does not survive a reboot, so scan the
// files the way systemd-sysctl applies them: the last assignment wins and must
// be at most 443. Unreadable files and unparsable values fail closed.
func (sp *SystemPlatformConfigurer) sysctlPersisted() bool {
	if sp.ReadFile == nil {
		return false
	}
	paths, ok := sp.sysctlFiles()
	if !ok {
		return false
	}
	last, found := 0, false
	for _, path := range paths {
		b, err := sp.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
			line = strings.TrimPrefix(line, "-") // systemd sysctl ignore-error prefix
			key, val, ok := strings.Cut(line, "=")
			key = strings.TrimSpace(key)
			if strings.HasPrefix(key, "/") {
				key = strings.ReplaceAll(strings.TrimPrefix(key, "/"), "/", ".")
			}
			if !ok || key != extenderSysctlKey {
				continue
			}
			v, err := strconv.Atoi(strings.TrimSpace(val))
			if err != nil {
				return false
			}
			last, found = v, true
		}
	}
	return found && last >= 0 && last <= 443
}

func (sp *SystemPlatformConfigurer) planLinuxSysctl() ([]setupCommand, error) {
	commands := []setupCommand{}
	exists, err := sp.sysctlDropinExists()
	if err != nil {
		return nil, err
	}
	if exists {
		commands = append(commands, setupCommand{name: "cp", args: []string{"-p", "--", extenderSysctlDropin, extenderSysctlBackup}})
	}
	return append(commands,
		setupCommand{name: "sh", args: []string{"-c", "echo " + extenderSysctlKey + "=443 > " + extenderSysctlDropin}},
		setupCommand{name: "sysctl", args: []string{"--system"}},
	), nil
}

func (sp *SystemPlatformConfigurer) applyLinuxSysctl() (applied bool, desc string, err error) {
	recovery := sp.SysctlUndo
	if recovery == "" {
		recovery, err = sp.sysctlRecovery()
		if err != nil {
			return false, "", err
		}
	}
	wrote := false
	defer func() {
		if wrote && err != nil {
			err = fmt.Errorf("%w; sysctl drop-in may be active; undo with: %s", err, recovery)
		}
	}()
	if sp.WriteFile == nil {
		return false, "", fmt.Errorf("cannot write sysctl config: WriteFile unavailable")
	}
	exists, err := sp.sysctlDropinExists()
	if err != nil {
		return false, "", err
	}
	if exists {
		if sp.Lstat != nil {
			if _, err := sp.Lstat(extenderSysctlBackup); err == nil {
				return false, "", fmt.Errorf("backup %s already exists; inspect it before retrying", extenderSysctlBackup)
			} else if !os.IsNotExist(err) {
				return false, "", fmt.Errorf("inspecting backup %s: %w", extenderSysctlBackup, err)
			}
		} else if _, err := sp.ReadFile(extenderSysctlBackup); err == nil {
			return false, "", fmt.Errorf("backup %s already exists; inspect it before retrying", extenderSysctlBackup)
		} else if !os.IsNotExist(err) {
			return false, "", fmt.Errorf("inspecting backup %s: %w", extenderSysctlBackup, err)
		}
		if sp.ExecCmd == nil {
			return false, "", fmt.Errorf("cannot back up %s: command runner unavailable", extenderSysctlDropin)
		}
		// cp -p keeps the mode, so a restored drop-in has its old permissions.
		if out, err := sp.ExecCmd("cp", "-p", "--", extenderSysctlDropin, extenderSysctlBackup); err != nil {
			return false, "", fmt.Errorf("backing up %s: %s: %w", extenderSysctlDropin, strings.TrimSpace(string(out)), err)
		}
	}
	wrote = true // even a partial write needs recovery guidance
	if err := sp.WriteFile(extenderSysctlDropin, []byte(extenderSysctlContent), 0o644); err != nil {
		return false, "", fmt.Errorf("writing %s: %w", extenderSysctlDropin, err)
	}
	if sp.ExecCmd != nil {
		out, err := sp.ExecCmd("sysctl", "--system")
		if err != nil {
			applyErr := fmt.Errorf("sysctl --system failed (%s: %w)", strings.TrimSpace(string(out)), err)
			out2, err2 := sp.ExecCmd("sysctl", "-w", extenderSysctlKey+"=443")
			if err2 != nil {
				return false, "", fmt.Errorf("applying sysctl failed: %v; fallback sysctl -w also failed: %s: %w", applyErr, strings.TrimSpace(string(out2)), err2)
			}
		}
	}
	if sp.ReadFile != nil {
		data, err := sp.ReadFile(extenderSysctlPortStartPath)
		if err != nil {
			return false, "", fmt.Errorf("verifying sysctl live value: %w", err)
		}
		val, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || val > 443 {
			return false, "", fmt.Errorf("sysctl live verification failed: %s is %s (want <= 443)", extenderSysctlKey, strings.TrimSpace(string(data)))
		}
		return true, fmt.Sprintf("applied sysctl drop-in %s (%s=%d); undo with: %s", extenderSysctlDropin, extenderSysctlKey, val, recovery), nil
	}
	return true, fmt.Sprintf("applied sysctl drop-in %s; undo with: %s", extenderSysctlDropin, recovery), nil
}

// --- FreeBSD implementation ---

func (sp *SystemPlatformConfigurer) checkFreeBSD(user string) (bool, string, error) {
	if user == "root" {
		return true, "provider running as root (no portrange.reservedhigh sysctl needed)", nil
	}
	out, err := sp.ExecCmd("sysctl", "-n", "net.inet.ip.portrange.reservedhigh")
	if err != nil {
		return false, fmt.Sprintf("sysctl net.inet.ip.portrange.reservedhigh failed: %v", err), nil
	}
	val := strings.TrimSpace(string(out))
	if val != "0" {
		return false, fmt.Sprintf("net.inet.ip.portrange.reservedhigh is %s (needs 0)", val), nil
	}

	// Check if persisted in /etc/sysctl.conf
	confBytes, err := sp.ReadFile("/etc/sysctl.conf")
	if err != nil {
		return false, "net.inet.ip.portrange.reservedhigh=0 live, but /etc/sysctl.conf unreadable", nil
	}
	if !sysctlConfSetsReservedHighZero(string(confBytes)) {
		return false, "net.inet.ip.portrange.reservedhigh=0 live, but not persisted in /etc/sysctl.conf", nil
	}

	return true, "net.inet.ip.portrange.reservedhigh=0 (live and persisted in /etc/sysctl.conf)", nil
}

// sysctlConfSetsReservedHighZero reports whether /etc/sysctl.conf, as the boot
// scripts read it, leaves net.inet.ip.portrange.reservedhigh at 0. Only live
// assignment lines count: a commented-out line does not persist anything, and
// when the file assigns the name more than once the last assignment wins.
func sysctlConfSetsReservedHighZero(content string) bool {
	const name = "net.inet.ip.portrange.reservedhigh"
	set := false
	for _, line := range strings.Split(content, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != name {
			continue
		}
		set = strings.TrimSpace(val) == "0"
	}
	return set
}

const freeBSDSetupInstructions = `FreeBSD extender setup is untested this release. No changes were made.
For a non-root provider, these temporary commands allow all users to bind
privileged ports. Run them in the same shell; stop if the first fails:
  previous=$(sysctl -n net.inet.ip.portrange.reservedhigh)
  sudo sysctl net.inet.ip.portrange.reservedhigh=0
Undo in that same shell:
  sudo sysctl net.inet.ip.portrange.reservedhigh="$previous"
The change is not persistent; setup does not edit /etc/sysctl.conf.
Manage your firewall manually: TCP 443 is required; UDP 443 and UDP 4053
are optional. UDP 53 is optional and user-managed. Check your pf or ipfw
rules and undo firewall changes using your own ruleset procedure.
`

func (sp *SystemPlatformConfigurer) applyFreeBSD(user string) (bool, string, error) {
	return false, "FreeBSD extender setup is manual only", nil
}

// --- Windows implementation ---

func (sp *SystemPlatformConfigurer) checkWindows() (bool, string, error) {
	tcp := windowsRuleExists(sp.ExecCmd, "URnetwork Extender")
	udp := windowsRuleExists(sp.ExecCmd, "URnetwork Extender UDP")
	return tcp && udp, fmt.Sprintf("Windows firewall rules: TCP present=%t, UDP present=%t", tcp, udp), nil
}

func (sp *SystemPlatformConfigurer) applyWindows() (bool, string, error) {
	applied, err := applyWindowsRules(sp.ExecCmd)
	if err != nil {
		return applied, "", err
	}
	return applied, "Windows Firewall TCP 443 and UDP 443,4053 rules configured", nil
}

// --- macOS (darwin) implementation ---

func (sp *SystemPlatformConfigurer) checkDarwin() (bool, string, error) {
	return false, "", fmt.Errorf("macOS extender setup cannot verify or install a root LaunchDaemon; configure and verify the provider service manually")
}

func (sp *SystemPlatformConfigurer) applyDarwin() (bool, string, error) {
	return sp.checkDarwin()
}

// UserCanBindTCP443 reports a conservative platform check for the current user.
// On Linux, the default configurer has no binary path here, so a non-root user
// returns false even when a particular provider binary has a bind capability.
// Use CheckRequirement with that binary path and VerifyBind to check a provider.
// A false result does not prove that binding would fail.
func UserCanBindTCP443(cfg PlatformConfigurer, user string) bool {
	switch cfg.Platform() {
	case "windows":
		// Windows has no privileged-port concept below 1024
		return true
	case "darwin":
		// macOS only allows root below 1024
		return user == "root" || os.Geteuid() == 0
	case "freebsd":
		if user == "root" || os.Geteuid() == 0 {
			return true
		}
		configured, _, _ := cfg.CheckRequirement("", user)
		return configured
	default: // "linux"
		if user == "root" || os.Geteuid() == 0 {
			return true
		}
		// On Linux, check if binary has capability
		configured, _, _ := cfg.CheckRequirement("", user)
		return configured
	}
}
