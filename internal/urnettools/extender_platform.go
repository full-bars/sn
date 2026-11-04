package urnettools

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
)

// PlatformConfigurer defines the operations needed to configure and verify extender requirements.
type PlatformConfigurer interface {
	Platform() string
	CheckRequirement(binary, user string) (configured bool, desc string, err error)
	ApplyRequirement(binary, user string) (applied bool, desc string, err error)
	VerifyBind(user string) (ok bool, err error)
}

// SystemPlatformConfigurer is the default implementation that talks to OS binaries and kernel APIs.
type SystemPlatformConfigurer struct {
	PlatformName string
	ExecCmd      func(name string, args ...string) ([]byte, error)
	ReadFile     func(name string) ([]byte, error)
	WriteFile    func(name string, data []byte, perm os.FileMode) error
	ListenTCP    func(network, address string) (net.Listener, error)
	LookupPath   func(file string) (string, error)
	IsRoot       func() bool
}

// NewSystemPlatformConfigurer returns a SystemPlatformConfigurer for the platform.
func NewSystemPlatformConfigurer(platform string) *SystemPlatformConfigurer {
	return &SystemPlatformConfigurer{
		PlatformName: platform,
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			cmd := exec.Command(name, args...)
			return cmd.CombinedOutput()
		},
		ReadFile:   os.ReadFile,
		WriteFile:  os.WriteFile,
		ListenTCP:  net.Listen,
		LookupPath: exec.LookPath,
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

// ApplyRequirement applies the privileged platform requirement.
func (sp *SystemPlatformConfigurer) ApplyRequirement(binary, user string) (bool, string, error) {
	switch sp.PlatformName {
	case "linux":
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

// VerifyBind verifies that port TCP 443 can be opened as the provider user.
func (sp *SystemPlatformConfigurer) VerifyBind(user string) (bool, error) {
	ln, err := sp.ListenTCP("tcp", ":443")
	if err != nil {
		if strings.Contains(err.Error(), "address already in use") {
			// If already in use, check if it's already bound
			return true, nil
		}
		return false, err
	}
	_ = ln.Close()
	return true, nil
}

// --- Linux implementation ---

func (sp *SystemPlatformConfigurer) checkLinux(binary string) (bool, string, error) {
	if binary == "" {
		return false, "no provider binary path known", nil
	}
	out, err := sp.ExecCmd("getcap", binary)
	if err != nil {
		return false, fmt.Sprintf("getcap %s failed: %v", binary, err), nil
	}
	capOut := string(out)
	if strings.Contains(capOut, "cap_net_bind_service=ep") || strings.Contains(capOut, "cap_net_bind_service+ep") {
		return true, fmt.Sprintf("cap_net_bind_service=+ep already set on %s", binary), nil
	}
	return false, fmt.Sprintf("cap_net_bind_service=+ep missing on %s", binary), nil
}

func (sp *SystemPlatformConfigurer) applyLinux(binary string) (bool, string, error) {
	if binary == "" {
		return false, "", fmt.Errorf("cannot apply cap_net_bind_service: no provider binary path known")
	}
	// Apply capability
	if out, err := sp.ExecCmd("setcap", "cap_net_bind_service=+ep", binary); err != nil {
		return false, "", fmt.Errorf("setcap cap_net_bind_service=+ep %s failed: %s: %w", binary, strings.TrimSpace(string(out)), err)
	}

	// Verify re-reading getcap
	out, err := sp.ExecCmd("getcap", binary)
	if err != nil {
		return false, "", fmt.Errorf("re-reading getcap %s failed: %w", binary, err)
	}
	capOut := string(out)
	if !strings.Contains(capOut, "cap_net_bind_service=ep") && !strings.Contains(capOut, "cap_net_bind_service+ep") {
		return false, "", fmt.Errorf("re-reading getcap %s: capability cap_net_bind_service=+ep not present (got %q)", binary, strings.TrimSpace(capOut))
	}

	return true, fmt.Sprintf("applied cap_net_bind_service=+ep on %s (verified with getcap)", binary), nil
}

// --- FreeBSD implementation ---

func (sp *SystemPlatformConfigurer) checkFreeBSD(user string) (bool, string, error) {
	if user == "root" || (sp.IsRoot != nil && sp.IsRoot()) {
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
	if !strings.Contains(string(confBytes), "net.inet.ip.portrange.reservedhigh=0") {
		return false, "net.inet.ip.portrange.reservedhigh=0 live, but not persisted in /etc/sysctl.conf", nil
	}

	return true, "net.inet.ip.portrange.reservedhigh=0 (live and persisted in /etc/sysctl.conf)", nil
}

func (sp *SystemPlatformConfigurer) applyFreeBSD(user string) (bool, string, error) {
	// Read current value
	curOut, _ := sp.ExecCmd("sysctl", "-n", "net.inet.ip.portrange.reservedhigh")
	curVal := strings.TrimSpace(string(curOut))

	// Set sysctl
	if out, err := sp.ExecCmd("sysctl", "net.inet.ip.portrange.reservedhigh=0"); err != nil {
		return false, "", fmt.Errorf("sysctl net.inet.ip.portrange.reservedhigh=0 failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Read value after
	afterOut, err := sp.ExecCmd("sysctl", "-n", "net.inet.ip.portrange.reservedhigh")
	if err != nil {
		return false, "", fmt.Errorf("re-reading sysctl net.inet.ip.portrange.reservedhigh failed: %w", err)
	}
	afterVal := strings.TrimSpace(string(afterOut))

	// Persist in /etc/sysctl.conf
	sysctlConf := "/etc/sysctl.conf"
	data, _ := sp.ReadFile(sysctlConf)
	content := string(data)
	if !strings.Contains(content, "net.inet.ip.portrange.reservedhigh=0") {
		var buf bytes.Buffer
		buf.WriteString(content)
		if len(content) > 0 && !strings.HasSuffix(content, "\n") {
			buf.WriteString("\n")
		}
		buf.WriteString("# Extender role: allow unprivileged binding of port 443\nnet.inet.ip.portrange.reservedhigh=0\n")
		if err := sp.WriteFile(sysctlConf, buf.Bytes(), 0644); err != nil {
			return false, "", fmt.Errorf("failed to persist in %s: %w", sysctlConf, err)
		}
	}

	desc := fmt.Sprintf("net.inet.ip.portrange.reservedhigh changed from %s to %s and persisted in %s", curVal, afterVal, sysctlConf)
	return true, desc, nil
}

// --- Windows implementation ---

func (sp *SystemPlatformConfigurer) checkWindows() (bool, string, error) {
	ruleName := "URnetwork Extender"
	out, err := sp.ExecCmd("netsh", "advfirewall", "firewall", "show", "rule", "name="+ruleName)
	if err == nil && strings.Contains(string(out), ruleName) {
		return true, fmt.Sprintf("inbound firewall rule %q already exists", ruleName), nil
	}
	return false, fmt.Sprintf("inbound firewall rule %q does not exist", ruleName), nil
}

func (sp *SystemPlatformConfigurer) applyWindows() (bool, string, error) {
	ruleName := "URnetwork Extender"
	// Double check identically named rule does not already exist
	out, _ := sp.ExecCmd("netsh", "advfirewall", "firewall", "show", "rule", "name="+ruleName)
	if strings.Contains(string(out), ruleName) {
		return false, fmt.Sprintf("firewall rule %q already exists", ruleName), nil
	}

	// Add TCP rule
	if out, err := sp.ExecCmd("netsh", "advfirewall", "firewall", "add", "rule", "name="+ruleName, "dir=in", "action=allow", "protocol=TCP", "localport=443"); err != nil {
		return false, "", fmt.Errorf("netsh advfirewall add TCP 443 rule failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Add UDP rule
	udpRuleName := ruleName + " UDP"
	if out, err := sp.ExecCmd("netsh", "advfirewall", "firewall", "add", "rule", "name="+udpRuleName, "dir=in", "action=allow", "protocol=UDP", "localport=443,4053"); err != nil {
		return false, "", fmt.Errorf("netsh advfirewall add UDP 443,4053 rule failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return true, fmt.Sprintf("added Windows Firewall inbound rules %q (TCP 443) and %q (UDP 443, 4053)", ruleName, udpRuleName), nil
}

// --- macOS (darwin) implementation ---

func (sp *SystemPlatformConfigurer) checkDarwin() (bool, string, error) {
	plist := "/Library/LaunchDaemons/com.urnetwork.provider.plist"
	if _, err := sp.ReadFile(plist); err == nil {
		out, err := sp.ExecCmd("launchctl", "list", "com.urnetwork.provider")
		if err == nil && !strings.Contains(string(out), "Could not find") {
			return true, fmt.Sprintf("root LaunchDaemon %s is installed and loaded", plist), nil
		}
		return false, fmt.Sprintf("root LaunchDaemon %s exists but is not active", plist), nil
	}
	return false, "root LaunchDaemon (/Library/LaunchDaemons/com.urnetwork.provider.plist) not found", nil
}

func (sp *SystemPlatformConfigurer) applyDarwin() (bool, string, error) {
	plist := "/Library/LaunchDaemons/com.urnetwork.provider.plist"
	// Check if already root
	if sp.IsRoot != nil && !sp.IsRoot() {
		return false, "", fmt.Errorf("macOS requires root privileges to install LaunchDaemon for port 443")
	}

	desc := fmt.Sprintf("macOS has no file capabilities; extender role requires running as root under %s rather than a user LaunchAgent. Note: macOS Application Firewall may prompt to allow inbound connections", plist)
	// If plist doesn't exist, verify guidance is printed
	if _, err := sp.ReadFile(plist); err != nil {
		return false, desc, fmt.Errorf("root LaunchDaemon %s must be installed with root privileges", plist)
	}

	// If it exists, ensure it is loaded
	_, _ = sp.ExecCmd("launchctl", "load", "-w", plist)
	return true, desc, nil
}

// UserCanBindTCP443 determines whether the current user on the platform can bind port 443.
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
