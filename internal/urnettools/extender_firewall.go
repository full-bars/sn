package urnettools

import (
	"fmt"
	"os/exec"
	"strings"
)

// FirewallKind identifies the active firewall manager.
type FirewallKind string

const (
	FirewallUnknown    FirewallKind = "unknown"
	FirewallNone       FirewallKind = "none"
	FirewallFirewalld  FirewallKind = "firewalld"
	FirewallUFW        FirewallKind = "ufw"
	FirewallNftables   FirewallKind = "nftables"
	FirewallIptables   FirewallKind = "iptables"
	FirewallPF         FirewallKind = "pf"
	FirewallIPFW       FirewallKind = "ipfw"
	FirewallWindowsAdv FirewallKind = "windows-advfirewall"
)

// FirewallStatus describes the detected firewall state and rules.
type FirewallStatus struct {
	Kind           FirewallKind `json:"kind"`
	Active         bool         `json:"active"`
	PortsOpen      bool         `json:"ports_open"`
	SuggestedRules []string     `json:"suggested_rules"`
	AppliedRules   []string     `json:"applied_rules,omitempty"`
	Message        string       `json:"message"`
	firewalldPlan  [][]string
}

// FirewallManager provides firewall detection and rule configuration.
type FirewallManager struct {
	Platform string
	ExecCmd  func(name string, args ...string) ([]byte, error)
	LookPath func(file string) (string, error)
}

// DefaultFirewallManager returns a FirewallManager for the given platform.
func DefaultFirewallManager(platform string) *FirewallManager {
	return &FirewallManager{
		Platform: platform,
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			cmd := exec.Command(name, args...)
			return cmd.CombinedOutput()
		},
		LookPath: exec.LookPath,
	}
}

// Detect checks the active firewall according to platform-specific ordering.
func (fm *FirewallManager) Detect() FirewallStatus {
	switch fm.Platform {
	case "windows":
		return fm.detectWindows()
	case "freebsd":
		return fm.detectFreeBSD()
	case "darwin":
		return fm.detectDarwin()
	default: // "linux"
		return fm.detectLinux()
	}
}

func (fm *FirewallManager) detectLinux() FirewallStatus {
	// 1. firewalld
	if _, err := fm.LookPath("firewall-cmd"); err == nil {
		if out, err := fm.ExecCmd("firewall-cmd", "--state"); err == nil && strings.TrimSpace(string(out)) == "running" {
			plan, planErr := firewalldPlan(fm.ExecCmd)
			open := planErr == nil && len(plan) == 0
			var rules []string
			for _, args := range plan {
				rules = append(rules, "firewall-cmd "+strings.Join(args, " "))
			}
			msg := "firewalld is active"
			if planErr != nil {
				msg = fmt.Sprintf("firewalld is active but its rules could not be inspected: %v; verify manually", planErr)
			} else if open {
				msg = "firewalld is active; ports 443/tcp, 443/udp, 4053/udp are open"
			}
			return FirewallStatus{
				Kind:           FirewallFirewalld,
				Active:         true,
				PortsOpen:      open,
				SuggestedRules: rules,
				Message:        msg,
				firewalldPlan:  plan,
			}
		}
	}

	// 2. ufw
	if _, err := fm.LookPath("ufw"); err == nil {
		out, err := fm.ExecCmd("ufw", "status")
		if err != nil {
			// `ufw status` needs root. An unprivileged caller cannot tell an
			// active ufw from an absent one, so report it as present and
			// unverified instead of falling through to "no firewall".
			return FirewallStatus{
				Kind:   FirewallUFW,
				Active: true,
				SuggestedRules: []string{
					"ufw allow 443/tcp",
					"ufw allow 443/udp",
					"ufw allow 4053/udp",
				},
				Message: "ufw is installed but its status is unreadable (needs root); treating it as active",
			}
		}
		{
			str := string(out)
			if strings.Contains(str, "Status: active") {
				// Check rules
				tcpOpen := ufwAllows(str, "443/tcp")
				udp443Open := ufwAllows(str, "443/udp")
				udp4053Open := ufwAllows(str, "4053/udp")
				open := tcpOpen && udp443Open && udp4053Open
				rules := []string{
					"ufw allow 443/tcp",
					"ufw allow 443/udp",
					"ufw allow 4053/udp",
				}
				msg := "ufw is active"
				if open {
					msg = "ufw is active; ports 443/tcp, 443/udp, 4053/udp are allowed"
				}
				return FirewallStatus{
					Kind:           FirewallUFW,
					Active:         true,
					PortsOpen:      open,
					SuggestedRules: rules,
					Message:        msg,
				}
			}
		}
	}

	// 3. nftables
	if _, err := fm.LookPath("nft"); err == nil {
		if fm.isSystemdServiceActive("nftables") {
			rules := []string{
				"nft add rule inet filter input tcp dport 443 accept",
				"nft add rule inet filter input udp dport { 443, 4053 } accept",
			}
			return FirewallStatus{
				Kind:           FirewallNftables,
				Active:         true,
				PortsOpen:      false, // cannot assume without parsing complex ruleset
				SuggestedRules: rules,
				Message:        "nftables service is active",
			}
		}
	}

	// 4. iptables
	if _, err := fm.LookPath("iptables"); err == nil {
		if fm.isSystemdServiceActive("iptables") {
			rules := []string{
				"iptables -I INPUT -p tcp --dport 443 -j ACCEPT",
				"iptables -I INPUT -p udp --dport 443 -j ACCEPT",
				"iptables -I INPUT -p udp --dport 4053 -j ACCEPT",
			}
			return FirewallStatus{
				Kind:           FirewallIptables,
				Active:         true,
				PortsOpen:      false,
				SuggestedRules: rules,
				Message:        "iptables service is active",
			}
		}
	}

	// Unknown / no active firewall service detected
	return FirewallStatus{
		Kind:      FirewallUnknown,
		Active:    false,
		PortsOpen: false,
		SuggestedRules: []string{
			"iptables: iptables -I INPUT -p tcp --dport 443 -j ACCEPT && iptables -I INPUT -p udp --dport 443 -j ACCEPT && iptables -I INPUT -p udp --dport 4053 -j ACCEPT",
			"nftables: nft add rule inet filter input tcp dport 443 accept && nft add rule inet filter input udp dport { 443, 4053 } accept",
		},
		Message: "no active firewall service detected (firewalld, ufw, nftables, iptables); verify host firewall manually",
	}
}

func ufwAllows(status, port string) bool {
	for _, line := range strings.Split(status, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == port && fields[1] == "ALLOW" && fields[2] == "Anywhere" {
			return true
		}
	}
	return false
}

func (fm *FirewallManager) isSystemdServiceActive(service string) bool {
	if _, err := fm.LookPath("systemctl"); err != nil {
		return false
	}
	out, err := fm.ExecCmd("systemctl", "is-active", service)
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

func (fm *FirewallManager) detectFreeBSD() FirewallStatus {
	// 1. pf
	if _, err := fm.LookPath("pfctl"); err == nil {
		out, err := fm.ExecCmd("pfctl", "-s", "info")
		if err == nil && strings.Contains(string(out), "Status: Enabled") {
			rules := []string{
				"pass in proto tcp to any port 443",
				"pass in proto udp to any port { 443 4053 }",
			}
			return FirewallStatus{
				Kind:           FirewallPF,
				Active:         true,
				PortsOpen:      false,
				SuggestedRules: rules,
				Message:        "pf is active; ensure /etc/pf.conf allows inbound tcp 443, udp 443, udp 4053",
			}
		}
	}

	// 2. ipfw
	if _, err := fm.LookPath("ipfw"); err == nil {
		out, err := fm.ExecCmd("service", "ipfw", "status")
		if err == nil && strings.Contains(strings.ToLower(string(out)), "is running") {
			rules := []string{
				"ipfw add allow tcp from any to any 443 in",
				"ipfw add allow udp from any to any 443,4053 in",
			}
			return FirewallStatus{
				Kind:           FirewallIPFW,
				Active:         true,
				PortsOpen:      false,
				SuggestedRules: rules,
				Message:        "ipfw is active; ensure rules allow inbound tcp 443, udp 443, udp 4053",
			}
		}
	}

	return FirewallStatus{
		Kind:      FirewallUnknown,
		Active:    false,
		PortsOpen: false,
		SuggestedRules: []string{
			"pf: pass in proto tcp to any port 443; pass in proto udp to any port { 443 4053 }",
			"ipfw: ipfw add allow tcp from any to any 443 in; ipfw add allow udp from any to any 443,4053 in",
		},
		Message: "no active firewall service detected (pf, ipfw); verify host firewall manually",
	}
}

func (fm *FirewallManager) detectDarwin() FirewallStatus {
	return FirewallStatus{
		Kind:      FirewallNone,
		Active:    true,
		PortsOpen: false,
		SuggestedRules: []string{
			"macOS Application Firewall (ALF) may prompt to allow inbound connections; confirm in System Settings > Network > Firewall",
		},
		Message: "macOS application firewall prompts interactively per binary; cannot be automated silently",
	}
}

func (fm *FirewallManager) detectWindows() FirewallStatus {
	ruleName := "URnetwork Extender"
	ruleExists := windowsRuleExists(fm.ExecCmd, ruleName) && windowsRuleExists(fm.ExecCmd, ruleName+" UDP")

	rules := []string{
		fmt.Sprintf(`netsh advfirewall firewall add rule name="%s" dir=in action=allow protocol=TCP localport=443`, ruleName),
		fmt.Sprintf(`netsh advfirewall firewall add rule name="%s UDP" dir=in action=allow protocol=UDP localport=443,4053`, ruleName),
	}

	msg := "Windows Firewall: rule does not exist"
	if ruleExists {
		msg = fmt.Sprintf("Windows Firewall: rule %q already exists", ruleName)
	}

	return FirewallStatus{
		Kind:           FirewallWindowsAdv,
		Active:         true,
		PortsOpen:      ruleExists,
		SuggestedRules: rules,
		Message:        msg,
	}
}

// windowsRuleExists reports whether the named inbound rule is present and
// usable. A rule that exists but is disabled or set to block is present and
// not usable: it must be repaired, not counted as configured.
func windowsRuleExists(run func(string, ...string) ([]byte, error), name string) bool {
	present, usable := windowsRuleState(run, name)
	return present && usable
}

// windowsRuleState parses `netsh advfirewall firewall show rule`. Only the
// fields it can read are judged: on a localized Windows the labels differ, and
// a field that cannot be found is not held against the rule.
func windowsRuleState(run func(string, ...string) ([]byte, error), name string) (present, usable bool) {
	out, err := run("netsh", "advfirewall", "firewall", "show", "rule", "name="+name)
	if err != nil {
		return false, false
	}
	usable = true
	inRule := false
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		i := strings.Index(line, ":")
		if i < 0 {
			if line == name {
				present, inRule = true, true
			}
			continue
		}
		key, val := strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:])
		if val == name && (strings.EqualFold(key, "Rule Name")) {
			present, inRule = true, true
			continue
		}
		if !inRule {
			if val == name {
				present, inRule = true, true
			}
			continue
		}
		switch {
		case strings.EqualFold(key, "Enabled") && strings.EqualFold(val, "No"):
			usable = false
		case strings.EqualFold(key, "Action") && strings.EqualFold(val, "Block"):
			usable = false
		case strings.EqualFold(key, "Direction") && strings.EqualFold(val, "Out"):
			usable = false
		}
	}
	return present, present && usable
}

func applyWindowsRules(run func(string, ...string) ([]byte, error)) (bool, error) {
	applied := false
	for _, rule := range []struct{ name, protocol, ports string }{
		{"URnetwork Extender", "TCP", "443"},
		{"URnetwork Extender UDP", "UDP", "443,4053"},
	} {
		present, usable := windowsRuleState(run, rule.name)
		if present && usable {
			continue
		}
		if present {
			// Disabled or blocking: remove it so the repaired rule is the only one.
			if out, err := run("netsh", "advfirewall", "firewall", "delete", "rule", "name="+rule.name); err != nil {
				return applied, fmt.Errorf("netsh delete unusable %s rule failed: %s: %w", rule.protocol, strings.TrimSpace(string(out)), err)
			}
		}
		out, err := run("netsh", "advfirewall", "firewall", "add", "rule", "name="+rule.name, "dir=in", "action=allow", "protocol="+rule.protocol, "localport="+rule.ports)
		if err != nil {
			return applied, fmt.Errorf("netsh add %s rule failed: %s: %w", rule.protocol, strings.TrimSpace(string(out)), err)
		}
		applied = true
	}
	return applied, nil
}

// firewalldPlan inspects both live and persistent rules before making a plan.
func firewalldPlan(run func(string, ...string) ([]byte, error)) ([][]string, error) {
	out, err := run("firewall-cmd", "--get-default-zone")
	zone := strings.TrimSpace(string(out))
	if err != nil || zone == "" || strings.IndexFunc(zone, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) >= 0 {
		return nil, fmt.Errorf("cannot determine firewalld default zone: output %q, error %v", zone, err)
	}
	var plan [][]string
	for _, port := range []string{"443/tcp", "443/udp", "4053/udp"} {
		for _, permanent := range []bool{false, true} {
			args := []string{"--zone=" + zone, "--query-port=" + port}
			if permanent {
				args = append([]string{"--permanent"}, args...)
			}
			out, err := run("firewall-cmd", args...)
			answer := strings.TrimSpace(string(out))
			if answer == "yes" && err == nil {
				continue
			}
			if answer != "no" {
				return nil, fmt.Errorf("cannot query firewalld port %s: output %q, error %v", port, answer, err)
			}
			args = []string{"--zone=" + zone, "--add-port=" + port}
			if permanent {
				args = append([]string{"--permanent"}, args...)
			}
			plan = append(plan, args)
		}
	}
	return plan, nil
}

// Apply attempts to apply the firewall configuration if possible with root/admin privileges.
// Returns (applied, alreadyOpen, error).
func (fm *FirewallManager) Apply(status FirewallStatus) (applied bool, err error) {
	switch status.Kind {
	case FirewallFirewalld:
		if status.PortsOpen {
			return false, nil
		}
		if len(status.firewalldPlan) == 0 {
			return false, fmt.Errorf("firewalld rules are unverified; no safe plan available")
		}
		applied := false
		for _, args := range status.firewalldPlan {
			if _, err := fm.ExecCmd("firewall-cmd", args...); err != nil {
				return applied, fmt.Errorf("firewall-cmd add-port failed: %w", err)
			}
			applied = true
		}
		return applied, nil

	case FirewallUFW:
		if status.PortsOpen {
			return false, nil
		}
		for _, portProto := range []string{"443/tcp", "443/udp", "4053/udp"} {
			if _, err := fm.ExecCmd("ufw", "allow", portProto); err != nil {
				return false, fmt.Errorf("ufw allow %s failed: %w", portProto, err)
			}
		}
		return true, nil

	case FirewallWindowsAdv:
		if status.PortsOpen {
			return false, nil
		}
		return applyWindowsRules(fm.ExecCmd)

	default:
		// For nftables, iptables, pf, ipfw, or unknown: do not silently succeed.
		// Return false with no error so the caller prints the exact suggested rules.
		return false, nil
	}
}
