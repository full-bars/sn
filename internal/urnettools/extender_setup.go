package urnettools

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// setupDiscoverFn is Discover behind a seam for tests.
var setupDiscoverFn = Discover

// setupCommand is a command and its exact arguments, without shell expansion.
type setupCommand struct {
	name string
	args []string
}

func setupShellWord(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_./:=-+", r))
	}) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func (c setupCommand) String() string {
	words := []string{setupShellWord(c.name)}
	for _, arg := range c.args {
		words = append(words, setupShellWord(arg))
	}
	return strings.Join(words, " ")
}

// executeExtenderSetupPlan prints and confirms the complete plan before running it.
func executeExtenderSetupPlan(out io.Writer, plan []setupCommand, yes, dryRun bool, confirm func() (bool, error), run func(setupCommand) error) error {
	if len(plan) == 0 {
		return nil
	}
	fmt.Fprintln(out, "Commands to run:")
	for _, c := range plan {
		fmt.Fprintln(out, "  "+c.String())
	}
	if dryRun {
		return nil
	}
	if !yes {
		ok, err := confirm()
		if err != nil {
			return fmt.Errorf("confirmation required; use --yes for non-interactive setup: %w", err)
		}
		if !ok {
			return fmt.Errorf("extender setup cancelled; no commands executed")
		}
	}
	for _, c := range plan {
		if err := run(c); err != nil {
			return fmt.Errorf("%s: %w", c.String(), err)
		}
	}
	return nil
}

func fallbackPlatformPlan(platform string, p Provider) []setupCommand {
	switch platform {
	case "linux":
		bin := p.Binary
		if bin == "" {
			bin = "/usr/local/bin/urnetwork"
		}
		return []setupCommand{{name: "setcap", args: []string{"cap_net_bind_service=+ep", bin}}}
	case "freebsd":
		if p.User == "root" {
			return nil
		}
		return []setupCommand{{name: "sysctl", args: []string{"net.inet.ip.portrange.reservedhigh=0"}}}
	case "windows":
		return []setupCommand{
			{name: "netsh", args: []string{"advfirewall", "firewall", "add", "rule", "name=URnetwork Extender", "dir=in", "action=allow", "protocol=TCP", "localport=443"}},
			{name: "netsh", args: []string{"advfirewall", "firewall", "add", "rule", "name=URnetwork Extender UDP", "dir=in", "action=allow", "protocol=UDP", "localport=443,4053"}},
		}
	default:
		return nil
	}
}

func planSetupCommands(platform string, platformCfg PlatformConfigurer, needPlatformTargets []Provider, fwStatus FirewallStatus) ([]setupCommand, []Provider, error) {
	var plan []setupCommand
	var plannedTargets []Provider
	seenCmd := map[string]bool{}
	addCmd := func(c setupCommand) {
		key := c.String()
		if !seenCmd[key] {
			seenCmd[key] = true
			plan = append(plan, c)
		}
	}
	for _, p := range needPlatformTargets {
		if planner, ok := platformCfg.(platformPlanner); ok {
			cmds, err := planner.PlanRequirement(p.Binary, p.User)
			if err != nil {
				return nil, nil, fmt.Errorf("planning platform requirement: %w", err)
			}
			for _, c := range cmds {
				addCmd(c)
			}
			if len(cmds) > 0 {
				plannedTargets = append(plannedTargets, p)
			}
			continue
		}
		fallbackCmds := fallbackPlatformPlan(platform, p)
		for _, c := range fallbackCmds {
			addCmd(c)
		}
		if len(fallbackCmds) > 0 {
			plannedTargets = append(plannedTargets, p)
		}
	}
	if !fwStatus.PortsOpen {
		switch fwStatus.Kind {
		case FirewallFirewalld:
			if len(fwStatus.firewalldPlan) > 0 {
				for _, args := range fwStatus.firewalldPlan {
					addCmd(setupCommand{name: "firewall-cmd", args: args})
				}
			} else {
				addCmd(setupCommand{name: "firewall-cmd", args: []string{"--permanent", "--add-port=443/tcp", "--add-port=443/udp", "--add-port=4053/udp"}})
				addCmd(setupCommand{name: "firewall-cmd", args: []string{"--reload"}})
			}
		case FirewallUFW:
			addCmd(setupCommand{name: "ufw", args: []string{"allow", "443/tcp"}})
			addCmd(setupCommand{name: "ufw", args: []string{"allow", "443/udp"}})
			addCmd(setupCommand{name: "ufw", args: []string{"allow", "4053/udp"}})
		case FirewallWindowsAdv:
			addCmd(setupCommand{name: "netsh", args: []string{"advfirewall", "firewall", "add", "rule", "name=URnetwork Extender", "dir=in", "action=allow", "protocol=TCP", "localport=443"}})
			addCmd(setupCommand{name: "netsh", args: []string{"advfirewall", "firewall", "add", "rule", "name=URnetwork Extender UDP", "dir=in", "action=allow", "protocol=UDP", "localport=443,4053"}})
		}
	}
	return plan, plannedTargets, nil
}

func computeExtenderSetupPlan(platform string, platformCfg PlatformConfigurer, fwMgr *FirewallManager, targets []Provider) ([]setupCommand, FirewallStatus, []Provider, bool, string, error) {
	configured := true
	var descs []string
	var needPlatformTargets []Provider
	for _, p := range targets {
		ok, desc, err := platformCfg.CheckRequirement(p.Binary, p.User)
		if err != nil {
			return nil, FirewallStatus{}, nil, false, "", fmt.Errorf("checking platform requirement: %w", err)
		}
		if !ok {
			configured = false
			needPlatformTargets = append(needPlatformTargets, p)
		}
		descs = append(descs, desc)
	}
	desc := strings.Join(descs, "; ")
	fwStatus := fwMgr.Detect()
	plan, plannedTargets, err := planSetupCommands(platform, platformCfg, needPlatformTargets, fwStatus)
	if err != nil {
		return nil, FirewallStatus{}, nil, false, "", err
	}
	return plan, fwStatus, plannedTargets, configured, desc, nil
}

func formatPlanText(plan []setupCommand) string {
	var sb strings.Builder
	for _, c := range plan {
		sb.WriteString(c.String())
		sb.WriteByte('\n')
	}
	return sb.String()
}

// ExtenderSetupOptions controls the behavior of `urnet-tools extender setup`.
type ExtenderSetupOptions struct {
	Yes            bool
	Force          bool
	NonInteractive bool
	DryRun         bool
	Target         Target
	Stdout         io.Writer
	Stderr         io.Writer

	// Injectable dependencies for deterministic testing
	Platform    string
	PortChecker *PortChecker
	PlatformCfg PlatformConfigurer
	FirewallMgr *FirewallManager
	IsRoot      func() bool
	LookPath    func(string) (string, error)
	ConfirmFunc func() (bool, error)
	ElevateFunc func(args []string) error
	Argv        []string
}

// RunExtenderSetup executes the extender setup workflow in order:
// 1. Check ports first (TCP 443 conflict aborts; UDP conflict warns & continues)
// 2. Detect platform requirement (if already configured, reports and changes nothing)
// 3. Check privileges (if not root and sudo unavailable/declined, prints exact command and exits non-zero)
// 4. Apply platform requirement
// 5. Detect and configure firewall (or print exact rules if unknown)
// 6. Verify with a real bind
func RunExtenderSetup(opts ExtenderSetupOptions) error {
	out := opts.Stdout
	if out == nil {
		out = os.Stdout
	}
	errOut := opts.Stderr
	if errOut == nil {
		errOut = os.Stderr
	}

	platform := opts.Platform
	if platform == "" {
		platform = runtime.GOOS
	}

	if platform == "freebsd" {
		fmt.Fprint(out, freeBSDSetupInstructions)
		return nil
	}

	checker := opts.PortChecker
	if checker == nil {
		checker = DefaultPortChecker(platform)
	}

	platformCfg := opts.PlatformCfg
	if platformCfg == nil {
		platformCfg = NewSystemPlatformConfigurer(platform)
	}

	fwMgr := opts.FirewallMgr
	if fwMgr == nil {
		fwMgr = DefaultFirewallManager(platform)
	}

	isRootFn := opts.IsRoot
	if isRootFn == nil {
		isRootFn = platformIsPrivileged
	}

	lookPathFn := opts.LookPath
	if lookPathFn == nil {
		lookPathFn = exec.LookPath
	}

	// Resolve the provider(s) this setup acts on. The command takes no flags,
	// so a host with several providers cannot be narrowed by the operator:
	// capabilities and the bind probe are per provider binary and user, so every
	// distinct provider is configured instead of refusing with an inventory the
	// operator has no flag to answer.
	targets, err := setupTargets(setupDiscoverFn(), opts.Target)
	if err != nil {
		return fmt.Errorf("resolving target provider: %w", err)
	}

	// STEP 1: Check ports first
	ports, err := checker.CheckPorts()
	if err != nil {
		return fmt.Errorf("checking port occupancy: %w", err)
	}

	// TCP 443 busy check: stop, name process, change nothing, exit non-zero. Never kill anything.
	holderHidden := false
	if ports.TCP443.Held {
		isTargetHolder := false
		for _, p := range targets {
			if !p.Running {
				continue
			}
			if p.PID > 0 && ports.TCP443.PID == p.PID {
				isTargetHolder = true
			} else if p.Binary != "" && filepath.Base(p.Binary) == ports.TCP443.Process {
				isTargetHolder = true
			}
		}
		// `ss -p` and `lsof` hide sockets owned by other users from an
		// unprivileged caller. A held port with no holder identity, seen before
		// elevation, is therefore not proof of a foreign process: a root provider
		// holding 443 reads exactly like this. Defer the verdict to the elevated
		// re-run, which sees every socket.
		holderHidden = !isRootFn() && ports.TCP443.Process == "" && ports.TCP443.PID == 0
		if holderHidden {
			fmt.Fprintf(errOut, "warn: TCP 443 is held by a process this user cannot see; re-checking after elevation\n")
		}
		if !isTargetHolder && !holderHidden {
			proc := ports.TCP443.Process
			if proc == "" && ports.TCP443.PID > 0 {
				proc = fmt.Sprintf("PID %d", ports.TCP443.PID)
			} else if proc != "" && ports.TCP443.PID > 0 {
				proc = fmt.Sprintf("%s (PID %d)", proc, ports.TCP443.PID)
			}
			if proc == "" {
				proc = "another process"
			}
			return fmt.Errorf("port TCP 443 is held by %s; extender role cannot run without it; please stop or reconfigure %s to free TCP 443", proc, proc)
		}
	}

	// UDP busy: warn and continue
	for _, w := range ports.UDPWarnings() {
		fmt.Fprintf(errOut, "warn: %s\n", w)
	}

	// STEP 2: Detect platform requirement and build initial plan
	plan, fwStatus, confirmedPlatformTargets, configured, desc, err := computeExtenderSetupPlan(platform, platformCfg, fwMgr, targets)
	if err != nil {
		return err
	}

	// No firewall service detected means there is nothing for setup to open;
	// it is reported as an advisory below, not treated as unconfigured, so a
	// re-run can reach the "already configured" path and the bind check.
	fwConfigured := fwStatus.PortsOpen || (!fwStatus.Active && (fwStatus.Kind == FirewallNone || fwStatus.Kind == FirewallUnknown))

	if fwStatus.Kind == FirewallUnknown && !opts.DryRun {
		fmt.Fprintf(errOut, "warn: %s\n", fwStatus.Message)
	}

	// Check if already configured on host
	if configured && fwConfigured && !holderHidden {
		if !opts.DryRun {
			for _, p := range targets {
				if err := reportExtenderBind(out, platformCfg, p, ports.TCP443.Held); err != nil {
					return err
				}
			}
		}
		fmt.Fprintf(out, "extender: already configured (%s)\n", desc)
		return nil
	}

	// STEP 3: Explain which ports and why
	fmt.Fprintln(out, "Extender ports: tcp 443 (required: TLS carrier), udp 443 (optional: QUIC carrier), udp 4053 (optional: DNS carrier)")

	// STEP 4: Privilege check
	if !isRootFn() {
		// Not root
		if len(plan) > 0 {
			_ = executeExtenderSetupPlan(out, plan, false, true, nil, func(setupCommand) error { return nil })
		}
		if opts.DryRun {
			fmt.Fprintf(out, "[dry-run] would apply platform requirement on %s\n", platform)
			fmt.Fprintf(out, "[dry-run] would configure firewall (%s)\n", fwStatus.Kind)
			return nil
		}

		exactCommand := formatSetupSudoCommand(opts)
		_, sudoErr := lookPathFn("sudo")
		sudoAvailable := sudoErr == nil

		if platform == "windows" {
			// There is no sudo and no re-exec: the operator opens an elevated
			// terminal and runs the same command.
			cmd := fmt.Sprintf("%s extender setup", resolvedExecutablePath())
			fmt.Fprintf(errOut, "extender setup requires Administrator privileges. Re-run from an elevated terminal (Run as administrator):\n  %s\n", cmd)
			return fmt.Errorf("extender setup requires Administrator privileges; run from an elevated terminal: %s", cmd)
		}
		if opts.NonInteractive || !stdinIsInteractive() || !sudoAvailable {
			fmt.Fprintf(errOut, "extender setup requires root privileges. Run:\n  %s\n", exactCommand)
			return fmt.Errorf("extender setup requires root privileges; run: %s", exactCommand)
		}

		// Interactive with sudo available: prompt operator
		if !opts.Yes && !opts.Force {
			fmt.Fprintf(errOut, "Extender setup requires privileged access to configure port binding and firewall rules.\n")
			ans, err := confirmStdinRead("Re-run under sudo now? [y/N] ")
			if err != nil || (strings.ToLower(strings.TrimSpace(ans)) != "y" && strings.ToLower(strings.TrimSpace(ans)) != "yes") {
				fmt.Fprintf(errOut, "Declined. You can run the setup command manually:\n  %s\n", exactCommand)
				return fmt.Errorf("declined privilege elevation; run manually: %s", exactCommand)
			}
		}

		// Re-exec under sudo
		elevateFn := opts.ElevateFunc
		if elevateFn == nil {
			elevateFn = elevateSelf
		}
		args := opts.Argv
		if len(args) == 0 {
			args = []string{"extender", "setup"}
		}
		return elevateFn(args)
	}

	// STEP 5: Dry-run or print-and-ask confirmation gate before mutating anything
	if opts.DryRun {
		if len(plan) > 0 {
			_ = executeExtenderSetupPlan(out, plan, false, true, nil, func(setupCommand) error { return nil })
		}
		fmt.Fprintf(out, "[dry-run] would apply platform requirement on %s\n", platform)
		fmt.Fprintf(out, "[dry-run] would configure firewall (%s)\n", fwStatus.Kind)
		return nil
	}

	if len(plan) > 0 {
		yes := opts.Yes || opts.Force
		confirmFn := opts.ConfirmFunc
		if confirmFn == nil {
			confirmFn = func() (bool, error) {
				if opts.NonInteractive || !stdinIsInteractive() {
					return false, fmt.Errorf("stdin is not a terminal; pass --yes (or -y) to confirm non-interactive setup")
				}
				ans, err := confirmStdinRead("Execute these commands? [y/N]: ")
				if err != nil {
					return false, err
				}
				trimmed := strings.ToLower(strings.TrimSpace(ans))
				return trimmed == "y" || trimmed == "yes", nil
			}
		}
		if err := executeExtenderSetupPlan(out, plan, yes, false, confirmFn, func(setupCommand) error { return nil }); err != nil {
			return err
		}

		// Drift guard: recompute plan immediately before applying.
		newPlan, newFwStatus, newConfirmedTargets, _, _, err := computeExtenderSetupPlan(platform, platformCfg, fwMgr, targets)
		if err != nil {
			return fmt.Errorf("recomputing plan before applying: %w", err)
		}
		if formatPlanText(newPlan) != formatPlanText(plan) {
			return fmt.Errorf("host configuration changed after confirmation; setup aborted without applying changes")
		}
		fwStatus = newFwStatus
		confirmedPlatformTargets = newConfirmedTargets
	}

	// STEP 6: Apply platform requirement and firewall rules
	for _, p := range confirmedPlatformTargets {
		applied, applyDesc, err := platformCfg.ApplyRequirement(p.Binary, p.User)
		if err != nil {
			return fmt.Errorf("applying platform requirement: %w", err)
		}
		if applied {
			fmt.Fprintf(out, "extender: %s\n", applyDesc)
		}
	}

	firewallUnverified := false
	fwApplied, err := fwMgr.Apply(fwStatus)
	if err != nil {
		return fmt.Errorf("firewall configuration failed: %w", err)
	} else if fwApplied {
		fmt.Fprintf(out, "extender: firewall rules added (%s)\n", fwStatus.Kind)
	} else if !fwStatus.PortsOpen && len(fwStatus.SuggestedRules) > 0 {
		fmt.Fprintf(out, "extender: no automated firewall modification for %s; add these rules manually:\n", fwStatus.Kind)
		for _, r := range fwStatus.SuggestedRules {
			fmt.Fprintf(out, "  %s\n", r)
		}
		// Not fatal: the rules cannot be checked or applied here, but the
		// capability and bind steps below are independent of them. Stopping
		// would make setup unfinishable on every nftables, iptables, pf or
		// ipfw host, and would skip the bind verification.
		firewallUnverified = true
	}

	// STEP 7: Verify with real bind
	for _, p := range targets {
		if err := reportExtenderBind(out, platformCfg, p, ports.TCP443.Held); err != nil {
			return err
		}
	}

	if firewallUnverified {
		fmt.Fprintf(out, "extender: setup complete, firewall NOT verified (%s); confirm inbound TCP 443, UDP 443, UDP 4053 are allowed\n", fwStatus.Kind)
		return nil
	}
	fmt.Fprintf(out, "extender: setup complete\n")
	return nil
}

func reportExtenderBind(out io.Writer, cfg PlatformConfigurer, p Provider, occupied bool) error {
	if occupied {
		fmt.Fprintln(out, "extender: TCP 443 bind unverified (port already held)")
		return nil
	}
	ok, err := cfg.VerifyBind(p.Binary, p.User)
	if err != nil {
		return fmt.Errorf("bind verification failed on TCP 443: %w", err)
	}
	if !ok {
		fmt.Fprintln(out, "extender: TCP 443 bind unverified (provider context unavailable)")
		return nil
	}
	fmt.Fprintln(out, "extender: verified TCP 443 listener as provider user")
	warnUnitNoNewPrivileges(out, cfg, p)
	return nil
}

// warnUnitNoNewPrivileges says so when the bind probe above cannot speak for
// the service: the probe ran outside the unit, and a unit with
// NoNewPrivileges makes the kernel ignore the file capability setup granted.
// Only a provider running as a non-root user depends on that capability.
func warnUnitNoNewPrivileges(out io.Writer, cfg PlatformConfigurer, p Provider) {
	if p.Unit == "" || p.User == "" || p.User == "root" {
		return
	}
	c, ok := cfg.(unitNoNewPrivileges)
	if !ok {
		return
	}
	if nnp, err := c.UnitNoNewPrivileges(p.Unit); err == nil && nnp {
		fmt.Fprintf(out, "warn: %s sets NoNewPrivileges, so the service will not receive the file capability that was just verified outside it; "+
			"TCP 443 may still fail to bind. Add AmbientCapabilities=CAP_NET_BIND_SERVICE to the unit (it works with NoNewPrivileges), or drop NoNewPrivileges\n", p.Unit)
	}
}

// formatSetupSudoCommand is the exact command to run by hand. `extender setup`
// takes no flags, so the command is the same on every host.
func formatSetupSudoCommand(ExtenderSetupOptions) string {
	return fmt.Sprintf("sudo %s extender setup", resolvedExecutablePath())
}

// setupTargets lists the providers setup configures: the explicit target when
// one is set (a resolution failure is an error, never a silent fallback), else
// the sole/sole-accessible/default provider, else every discovered provider
// once per distinct binary and user, else one anonymous provider whose binary
// is looked up in the usual install locations.
func setupTargets(providers []Provider, t Target) ([]Provider, error) {
	var out []Provider
	switch {
	case t != (Target{}):
		p, _, err := selectTargetOrSoleAccessible(providers, t, false)
		if err != nil {
			return nil, err
		}
		out = []Provider{p}
	case len(providers) == 0:
		out = []Provider{{}}
	default:
		if p, _, err := selectTargetOrSoleAccessible(providers, t, false); err == nil {
			out = []Provider{p}
		} else {
			seen := map[string]bool{}
			for _, p := range providers {
				bin := p.Binary
				if bin == "" {
					bin = findProviderBinary(p)
				}
				key := bin + "\x00" + p.User
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, p)
			}
		}
	}
	for i := range out {
		if out[i].Binary == "" {
			out[i].Binary = findProviderBinary(out[i])
		}
	}
	return out, nil
}

func findProviderBinary(p Provider) string {
	if p.Binary != "" {
		return p.Binary
	}
	candidates := []string{
		"/usr/local/bin/urnetwork",
		"/usr/bin/urnetwork",
		"/usr/local/bin/provider",
		"/usr/bin/provider",
		"/usr/local/bin/miner",
		"/usr/bin/miner",
		"/usr/local/libexec/urnetwork-provider/bin/urnetwork",
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".local", "share", "urnetwork-provider", "bin", "urnetwork"),
			filepath.Join(home, ".local", "bin", "urnetwork"),
			filepath.Join(home, ".urnetwork", "urnetwork"),
		)
	}
	if p.User != "" && p.User != "root" {
		candidates = append(candidates,
			filepath.Join("/home", p.User, ".local", "share", "urnetwork-provider", "bin", "urnetwork"),
			filepath.Join("/home", p.User, ".local", "bin", "urnetwork"),
			filepath.Join("/home", p.User, ".urnetwork", "urnetwork"),
		)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	for _, name := range []string{"urnetwork", "provider", "miner"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return ""
}
