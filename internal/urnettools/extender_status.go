package urnettools

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

// Extender reading source names.
const (
	SourceControlSocket  = "control socket"
	SourcePersistedState = "persisted state"
	SourceLog            = "events.log"
	SourceUnknown        = "unknown"
)

// ExtenderReading represents a comprehensive reading of extender status,
// resolved in strict fallback order:
// 1. Control socket (live provider)
// 2. Persisted state (provider_state.db)
// 3. events.log (historical record)
// 4. Unknown (when all sources fail)
type ExtenderReading struct {
	Source        string `json:"source"`
	Preference    string `json:"preference"`
	RoleState     string `json:"role_state"`
	Bound         bool   `json:"bound"`
	CarriersBound string `json:"carriers_bound,omitempty"`
	ActivatedV4   bool   `json:"activated_v4"`
	ActivatedV6   bool   `json:"activated_v6"`
	Ipv4          string `json:"ipv4,omitempty"`
	Ipv6          string `json:"ipv6,omitempty"`
	LastRefusal   string `json:"last_refusal,omitempty"`
	NextAttempt   string `json:"next_attempt,omitempty"`

	// Metric counters that exist on the pinned sdk in go.mod today:
	DnsPorts              string `json:"dns_ports,omitempty"`
	ConnectionCount       int    `json:"connection_count,omitempty"`
	PeerPingCount         int    `json:"peer_ping_count,omitempty"`
	PeerPingCosignedCount int    `json:"peer_ping_cosigned_count,omitempty"`
	PeerPingRejectedCount int    `json:"peer_ping_rejected_count,omitempty"`
	PeerPingUnknownCount  int    `json:"peer_ping_unknown_count,omitempty"`
	LastPeerPingTime      int64  `json:"last_peer_ping_time,omitempty"`
	LimitedBySubnetsCount int    `json:"limited_by_subnets_count,omitempty"`
	LimitedBySourceCount  int    `json:"limited_by_source_count,omitempty"`

	// Placeholders for serve counters in unmerged SDK PR #6:
	// TODO(sdk#6): Connected, ServedProcess, ServedLifetime, PeakConnected, LastServed
	// live in an unmerged sdk PR and are not in the pinned module.
	ServeCountersAvailable    bool  `json:"serve_counters_available"`
	ConnectedPlaceholder      int   `json:"connected_placeholder,omitempty"`
	ServedProcessPlaceholder  int64 `json:"served_process_placeholder,omitempty"`
	ServedLifetimePlaceholder int64 `json:"served_lifetime_placeholder,omitempty"`
	PeakConnectedPlaceholder  int   `json:"peak_connected_placeholder,omitempty"`
	LastServedPlaceholder     int64 `json:"last_served_placeholder,omitempty"`
}

// Package-level seams for reading extender status in fallback order.
var (
	socketExtenderFn = queryExtenderFromSocket
	stateExtenderFn  = readExtenderFromState
	logExtenderFn    = readExtenderFromLog
)

// ExtenderStatusReport is the data structure for `urnet-tools extender status`.
type ExtenderStatusReport struct {
	Platform       string          `json:"platform"`
	CanBindTCP443  bool            `json:"can_bind_tcp_443"`
	Ports          ExtenderPorts   `json:"ports"`
	RolePreference string          `json:"role_preference"` // "on" or "off"
	Bound          bool            `json:"bound"`
	Activated      bool            `json:"activated"`
	Healthy        bool            `json:"healthy"`
	Warnings       []string        `json:"warnings,omitempty"`
	Errors         []string        `json:"errors,omitempty"`
	Remedy         string          `json:"remedy,omitempty"`
	Firewall       *FirewallStatus `json:"firewall,omitempty"`
	ProviderTarget string          `json:"provider_target,omitempty"`
	Reading        ExtenderReading `json:"reading,omitempty"`
}

// ReadRolePreference reads the extender role preference for a provider.
func ReadRolePreference(stateDir string) string {
	if stateDir == "" {
		home, _ := os.UserHomeDir()
		stateDir = filepath.Join(home, ".urnetwork")
	}
	togglePath := filepath.Join(stateDir, "extender")
	data, err := os.ReadFile(togglePath)
	if err != nil {
		return "off"
	}
	val := strings.ToLower(strings.TrimSpace(string(data)))
	switch val {
	case "on", "1", "true", "yes", "enabled":
		return "on"
	default:
		return "off"
	}
}

// WriteRolePreference writes the extender role preference for a provider.
func WriteRolePreference(stateDir string, enabled bool) error {
	if stateDir == "" {
		home, _ := os.UserHomeDir()
		stateDir = filepath.Join(home, ".urnetwork")
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	togglePath := filepath.Join(stateDir, "extender")
	val := "off\n"
	if enabled {
		val = "on\n"
	}
	return os.WriteFile(togglePath, []byte(val), 0600)
}

// queryExtenderFromSocket reads extender observation from the control socket.
func queryExtenderFromSocket(p Provider) (*ExtenderReading, error) {
	if p.StateDir == "" {
		return nil, errors.New("no state directory")
	}
	sockPath := filepath.Join(p.StateDir, "provider.sock")
	resp, err := sendSocketRequest(sockPath, controlRequest{Cmd: "status"})
	if err != nil || !resp.OK || resp.Extender == nil || !resp.Extender.Known {
		resp2, err2 := sendSocketRequest(sockPath, controlRequest{Cmd: "extender"})
		if err2 != nil || !resp2.OK || resp2.Extender == nil || !resp2.Extender.Known {
			if err != nil {
				return nil, err
			}
			return nil, errors.New("extender observation not available on socket")
		}
		resp = resp2
	}
	ex := resp.Extender
	pref := ReadRolePreference(p.StateDir)

	roleState := "disabled"
	if ex.Enabled {
		switch {
		case ex.ActivatedV4 || ex.ActivatedV6:
			roleState = "active"
		case ex.ActivationRefused:
			roleState = "refused"
		case ex.Listening:
			roleState = "listening"
		case ex.ListenFailed:
			roleState = "listen failed"
		default:
			roleState = "enabled"
		}
	}

	carriers := []string{}
	if ex.Listening {
		carriers = append(carriers, "tcp:443")
	}
	if ex.DnsPorts != "" {
		for _, port := range strings.Split(ex.DnsPorts, ",") {
			port = strings.TrimSpace(port)
			if port != "" {
				carriers = append(carriers, "dns:"+port)
			}
		}
	}
	carriersStr := strings.Join(carriers, ", ")
	if carriersStr == "" {
		if ex.Listening {
			carriersStr = "tcp:443"
		} else {
			carriersStr = "none"
		}
	}

	lastRefusal := "none"
	if ex.ActivationRefused {
		if ex.LastActivationError != "" {
			lastRefusal = ex.LastActivationError
		} else {
			lastRefusal = "refused by network"
		}
		if ex.ActivationTime > 0 {
			lastRefusal += fmt.Sprintf(" (%s)", time.Unix(ex.ActivationTime/1000, 0).Format("15:04:05"))
		}
	}

	nextAttempt := "immediate"
	if ex.ActivationRefused {
		nextAttempt = "in backoff"
	}

	return &ExtenderReading{
		Source:                SourceControlSocket,
		Preference:            pref,
		RoleState:             roleState,
		Bound:                 ex.Listening,
		CarriersBound:         carriersStr,
		ActivatedV4:           ex.ActivatedV4,
		ActivatedV6:           ex.ActivatedV6,
		Ipv4:                  ex.Ipv4,
		Ipv6:                  ex.Ipv6,
		LastRefusal:           lastRefusal,
		NextAttempt:           nextAttempt,
		DnsPorts:              ex.DnsPorts,
		ConnectionCount:       ex.ConnectionCount,
		PeerPingCount:         ex.PeerPingCount,
		PeerPingCosignedCount: ex.PeerPingCosignedCount,
		PeerPingRejectedCount: ex.PeerPingRejectedCount,
		PeerPingUnknownCount:  ex.PeerPingUnknownCount,
		LastPeerPingTime:      ex.LastPeerPingTime,
		LimitedBySubnetsCount: ex.LimitedBySubnetsCount,
		LimitedBySourceCount:  ex.LimitedBySourceCount,
	}, nil
}

// readExtenderFromState reads extender activation records from persisted state (provider_state.db).
func readExtenderFromState(p Provider) (*ExtenderReading, error) {
	if p.StateDir == "" {
		return nil, errors.New("no state directory")
	}
	dbPath := filepath.Join(p.StateDir, "provider_state.db")
	db, err := bbolt.Open(dbPath, 0600, &bbolt.Options{ReadOnly: true, Timeout: 100 * time.Millisecond})
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var recordBytes []byte
	err = db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("state"))
		if b == nil {
			return errors.New("no state bucket")
		}
		v := b.Get([]byte("extender_activation"))
		if v == nil {
			return errors.New("no extender_activation key")
		}
		recordBytes = make([]byte, len(v))
		copy(recordBytes, v)
		return nil
	})
	if err != nil {
		return nil, err
	}

	type activationRecord struct {
		Accounts map[string]struct {
			Attempts         []int64 `json:"attempts,omitempty"`
			BackoffUntilUnix int64   `json:"backoff_until_unix,omitempty"`
			Refusals         int     `json:"refusals,omitempty"`
			LastRefusalUnix  int64   `json:"last_refusal_unix,omitempty"`
		} `json:"accounts,omitempty"`
		QuarantineUntilUnix int64   `json:"quarantine_until_unix,omitempty"`
		Attempts            []int64 `json:"attempts,omitempty"`
		BackoffUntilUnix    int64   `json:"backoff_until_unix,omitempty"`
		Refusals            int     `json:"refusals,omitempty"`
		LastRefusalUnix     int64   `json:"last_refusal_unix,omitempty"`
	}

	var rec activationRecord
	if err := json.Unmarshal(recordBytes, &rec); err != nil {
		return nil, err
	}

	lastRefusalUnix := rec.LastRefusalUnix
	backoffUntilUnix := rec.BackoffUntilUnix
	refusals := rec.Refusals
	totalAttempts := len(rec.Attempts)

	for _, acc := range rec.Accounts {
		if acc.LastRefusalUnix > lastRefusalUnix {
			lastRefusalUnix = acc.LastRefusalUnix
		}
		if acc.BackoffUntilUnix > backoffUntilUnix {
			backoffUntilUnix = acc.BackoffUntilUnix
		}
		refusals += acc.Refusals
		totalAttempts += len(acc.Attempts)
	}

	if lastRefusalUnix == 0 && backoffUntilUnix == 0 && totalAttempts == 0 && len(rec.Accounts) == 0 {
		return nil, errors.New("empty extender activation record")
	}

	pref := ReadRolePreference(p.StateDir)
	roleState := "stopped"
	nowUnix := time.Now().Unix()
	if backoffUntilUnix > nowUnix {
		roleState = "refused (persisted backoff)"
	} else if p.Running {
		roleState = "inactive (persisted state)"
	}

	lastRefusal := "none"
	if lastRefusalUnix > 0 {
		lastRefusal = time.Unix(lastRefusalUnix, 0).Format("2006-01-02 15:04:05")
		if refusals > 0 {
			lastRefusal += fmt.Sprintf(" (%d refusals)", refusals)
		}
	}

	nextAttempt := "immediate"
	if backoffUntilUnix > nowUnix {
		wait := time.Duration(backoffUntilUnix-nowUnix) * time.Second
		nextAttempt = fmt.Sprintf("in %s", wait.Round(time.Second))
	} else if backoffUntilUnix > 0 {
		nextAttempt = "immediate (backoff elapsed)"
	}

	return &ExtenderReading{
		Source:        SourcePersistedState,
		Preference:    pref,
		RoleState:     roleState,
		Bound:         false,
		CarriersBound: "none",
		ActivatedV4:   false,
		ActivatedV6:   false,
		LastRefusal:   lastRefusal,
		NextAttempt:   nextAttempt,
	}, nil
}

// readExtenderFromLog reads the most recent extender observation from events.log.
// Never reports a stale log value as live.
func readExtenderFromLog(p Provider) (*ExtenderReading, error) {
	if p.StateDir == "" {
		return nil, errors.New("no state directory")
	}
	eventsPath := filepath.Join(p.StateDir, "events.log")
	f, err := os.Open(eventsPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	type diagEvent struct {
		Domain   string `json:"domain"`
		Event    string `json:"event"`
		Cause    string `json:"cause"`
		Extender *struct {
			Known               bool   `json:"known"`
			Enabled             bool   `json:"enabled"`
			Listening           bool   `json:"listening"`
			ActivatedV4         bool   `json:"activated_v4"`
			ActivatedV6         bool   `json:"activated_v6"`
			ActivationRefused   bool   `json:"activation_refused"`
			Ipv4                string `json:"ipv4"`
			Ipv6                string `json:"ipv6"`
			LastActivationError string `json:"last_activation_error"`
			DnsPorts            string `json:"dns_ports"`
			ConnectionCount     int    `json:"connection_count"`
		} `json:"extender"`
	}

	var latestEvent *diagEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.Contains(line, []byte(`"domain":"extender"`)) {
			continue
		}
		var ev diagEvent
		if err := json.Unmarshal(line, &ev); err == nil && ev.Extender != nil {
			evCopy := ev
			latestEvent = &evCopy
		}
	}

	if latestEvent == nil || latestEvent.Extender == nil {
		return nil, errors.New("no extender records in events.log")
	}

	pref := ReadRolePreference(p.StateDir)
	ex := latestEvent.Extender

	// Explicitly mark historical to never confuse stale log with live state
	roleState := "historical (events.log)"
	if ex.ActivationRefused {
		roleState = "refused (events.log)"
	}

	lastRefusal := "none"
	if ex.ActivationRefused {
		if ex.LastActivationError != "" {
			lastRefusal = ex.LastActivationError
		} else if latestEvent.Cause != "" {
			lastRefusal = latestEvent.Cause
		} else {
			lastRefusal = "refused"
		}
	}

	carriers := "none"
	if ex.Listening {
		carriers = "tcp:443 (historical)"
	}

	return &ExtenderReading{
		Source:          SourceLog,
		Preference:      pref,
		RoleState:       roleState,
		Bound:           false, // not currently live
		CarriersBound:   carriers,
		ActivatedV4:     false, // not currently live
		ActivatedV6:     false,
		Ipv4:            ex.Ipv4,
		Ipv6:            ex.Ipv6,
		LastRefusal:     lastRefusal,
		NextAttempt:     "unknown",
		DnsPorts:        ex.DnsPorts,
		ConnectionCount: ex.ConnectionCount,
	}, nil
}

// ReadExtenderStatus returns the extender reading, strictly checking:
// 1. Control socket (live provider)
// 2. Persisted state (provider_state.db)
// 3. events.log (historical)
// 4. Unknown (when none respond)
func ReadExtenderStatus(p Provider) ExtenderReading {
	if reading, err := socketExtenderFn(p); err == nil && reading != nil {
		return *reading
	}
	if reading, err := stateExtenderFn(p); err == nil && reading != nil {
		return *reading
	}
	if reading, err := logExtenderFn(p); err == nil && reading != nil {
		return *reading
	}
	return ExtenderReading{
		Source:        SourceUnknown,
		Preference:    ReadRolePreference(p.StateDir),
		RoleState:     "unknown",
		Bound:         false,
		CarriersBound: "none",
		ActivatedV4:   false,
		ActivatedV6:   false,
		LastRefusal:   "none",
		NextAttempt:   "unknown",
	}
}

// CheckActivation checks whether the provider role is activated from its state/logs.
func CheckActivation(p Provider) (bound, activated bool, detail string) {
	if !p.Running {
		return false, false, "provider is not running"
	}
	reading := ReadExtenderStatus(p)
	bound = reading.Bound
	activated = reading.ActivatedV4 || reading.ActivatedV6
	if reading.LastRefusal != "" && reading.LastRefusal != "none" {
		detail = "activation refused: " + reading.LastRefusal
	}
	return bound, activated, detail
}

// GatherExtenderStatus collects the status report without mutating system state.
func GatherExtenderStatus(
	platform string,
	target Provider,
	checker *PortChecker,
	platformCfg PlatformConfigurer,
	fwMgr *FirewallManager,
) (ExtenderStatusReport, error) {
	if platform == "" {
		platform = runtime.GOOS
	}
	if checker == nil {
		checker = DefaultPortChecker(platform)
	}
	if platformCfg == nil {
		platformCfg = NewSystemPlatformConfigurer(platform)
	}
	if fwMgr == nil {
		fwMgr = DefaultFirewallManager(platform)
	}

	ports, err := checker.CheckPorts()
	if err != nil {
		return ExtenderStatusReport{}, fmt.Errorf("checking ports: %w", err)
	}

	reading := ReadExtenderStatus(target)

	report := ExtenderStatusReport{
		Platform:       platform,
		Ports:          ports,
		RolePreference: ReadRolePreference(target.StateDir),
		ProviderTarget: providerLabel(target),
		Reading:        reading,
	}

	// Can user bind TCP 443
	user := target.User
	if user == "" {
		user = os.Getenv("USER")
	}
	canBind, _ := CanBindTCP443(nil)
	if !canBind {
		canBind = UserCanBindTCP443(platformCfg, user)
	}
	report.CanBindTCP443 = canBind

	// Bound & Activated
	if target.Running {
		if ports.TCP443.Held {
			if target.PID > 0 && ports.TCP443.PID == target.PID {
				report.Bound = true
			} else if target.Binary != "" && filepath.Base(target.Binary) == ports.TCP443.Process {
				report.Bound = true
			}
		}
		if reading.Bound {
			report.Bound = true
		}
		if reading.ActivatedV4 || reading.ActivatedV6 {
			report.Activated = true
		}
		if reading.LastRefusal != "" && reading.LastRefusal != "none" && !report.Activated {
			report.Warnings = append(report.Warnings, "refusal: "+reading.LastRefusal)
		}
	}

	// Firewall
	fwStatus := fwMgr.Detect()
	report.Firewall = &fwStatus

	// Diagnose errors and build remedy
	remedies := []string{}

	// 1. TCP 443 conflict
	if ports.TCP443.Held {
		isTargetHolder := false
		if target.PID > 0 && ports.TCP443.PID == target.PID {
			isTargetHolder = true
		} else if target.Binary != "" && filepath.Base(target.Binary) == ports.TCP443.Process {
			isTargetHolder = true
		}
		if !isTargetHolder {
			proc := ports.TCP443.Process
			if proc == "" && ports.TCP443.PID > 0 {
				proc = fmt.Sprintf("PID %d", ports.TCP443.PID)
			} else if proc != "" && ports.TCP443.PID > 0 {
				proc = fmt.Sprintf("%s (PID %d)", proc, ports.TCP443.PID)
			}
			report.Errors = append(report.Errors, fmt.Sprintf("TCP 443 is held by %s; extender cannot run without it", proc))
			remedies = append(remedies, fmt.Sprintf("Stop competing process %s or reconfigure it to free TCP 443", proc))
		}
	}

	// 2. Privilege / bind requirement
	if !report.CanBindTCP443 {
		switch platform {
		case "linux":
			report.Errors = append(report.Errors, "user cannot bind TCP 443 (missing cap_net_bind_service=+ep on provider binary)")
			remedies = append(remedies, "Run 'sudo urnet-tools extender setup' to grant cap_net_bind_service=+ep")
		case "freebsd":
			report.Errors = append(report.Errors, "user cannot bind TCP 443 (net.inet.ip.portrange.reservedhigh is non-zero)")
			remedies = append(remedies, "Run 'sudo urnet-tools extender setup' to set net.inet.ip.portrange.reservedhigh=0")
		case "darwin":
			report.Errors = append(report.Errors, "binding ports below 1024 on macOS requires root privileges")
			remedies = append(remedies, "Run provider as a root LaunchDaemon in /Library/LaunchDaemons/com.urnetwork.provider.plist")
		case "windows":
			// Windows allows any user to bind
		}
	}

	// 3. Role preference
	if report.RolePreference != "on" {
		report.Warnings = append(report.Warnings, "extender role preference is off")
		remedies = append(remedies, "Run 'urnet-tools extender setup --enable' to enable extender for this provider")
	}

	// 4. UDP warnings
	for _, w := range ports.UDPWarnings() {
		report.Warnings = append(report.Warnings, w)
	}

	// 5. Firewall checks
	if fwStatus.Active && !fwStatus.PortsOpen && len(fwStatus.SuggestedRules) > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("firewall (%s) does not have extender ports open", fwStatus.Kind))
		remedies = append(remedies, fmt.Sprintf("Allow ports in %s: %s", fwStatus.Kind, strings.Join(fwStatus.SuggestedRules, "; ")))
	}

	if len(report.Errors) == 0 {
		report.Healthy = true
	} else {
		report.Healthy = false
	}

	if len(remedies) > 0 {
		report.Remedy = strings.Join(remedies, ". ")
	}

	return report, nil
}

// formatExtenderBlock formats the extender block for `urnet-tools status` and `top`.
func formatExtenderBlock(w io.Writer, reading ExtenderReading) {
	fmt.Fprintf(w, "\nExtender:\n")
	fmt.Fprintf(w, "  Source:            %s\n", reading.Source)
	fmt.Fprintf(w, "  Preference:        %s\n", reading.Preference)
	fmt.Fprintf(w, "  Role state:        %s\n", reading.RoleState)
	carriers := reading.CarriersBound
	if carriers == "" {
		carriers = "none"
	}
	fmt.Fprintf(w, "  Carriers bound:    %s\n", carriers)

	v4Str := "inactive"
	if reading.ActivatedV4 {
		v4Str = "active"
		if reading.Ipv4 != "" {
			v4Str += fmt.Sprintf(" (%s)", reading.Ipv4)
		}
	} else if reading.Ipv4 != "" {
		v4Str = fmt.Sprintf("inactive (last: %s)", reading.Ipv4)
	} else if reading.Source == SourceUnknown {
		v4Str = "unknown"
	}
	fmt.Fprintf(w, "  IPv4 activation:   %s\n", v4Str)

	v6Str := "inactive"
	if reading.ActivatedV6 {
		v6Str = "active"
		if reading.Ipv6 != "" {
			v6Str += fmt.Sprintf(" (%s)", reading.Ipv6)
		}
	} else if reading.Ipv6 != "" {
		v6Str = fmt.Sprintf("inactive (last: %s)", reading.Ipv6)
	} else if reading.Source == SourceUnknown {
		v6Str = "unknown"
	}
	fmt.Fprintf(w, "  IPv6 activation:   %s\n", v6Str)

	lastRefusal := reading.LastRefusal
	if lastRefusal == "" {
		lastRefusal = "none"
	}
	fmt.Fprintf(w, "  Last refusal:      %s\n", lastRefusal)

	nextAttempt := reading.NextAttempt
	if nextAttempt == "" {
		nextAttempt = "none"
	}
	fmt.Fprintf(w, "  Next attempt:      %s\n", nextAttempt)

	if reading.ConnectionCount > 0 || reading.PeerPingCount > 0 || reading.LimitedBySubnetsCount > 0 || reading.LimitedBySourceCount > 0 {
		fmt.Fprintf(w, "  Connections:       %d\n", reading.ConnectionCount)
		if reading.PeerPingCount > 0 {
			fmt.Fprintf(w, "  Peer pings:        %d (cosigned: %d, rejected: %d, unknown: %d)\n",
				reading.PeerPingCount, reading.PeerPingCosignedCount, reading.PeerPingRejectedCount, reading.PeerPingUnknownCount)
		}
		if reading.LimitedBySubnetsCount > 0 || reading.LimitedBySourceCount > 0 {
			fmt.Fprintf(w, "  Rate limits:       subnets: %d, source: %d\n", reading.LimitedBySubnetsCount, reading.LimitedBySourceCount)
		}
	}
}

// FormatExtenderStatus formats the report as human-readable text or JSON.
func FormatExtenderStatus(w io.Writer, report ExtenderStatusReport, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	fmt.Fprintf(w, "Extender Status:\n")
	fmt.Fprintf(w, "  Platform:          %s\n", report.Platform)
	canBindStr := "yes"
	if !report.CanBindTCP443 {
		canBindStr = "no (requires privileged configure)"
	}
	fmt.Fprintf(w, "  Can bind TCP 443:  %s\n", canBindStr)
	fmt.Fprintf(w, "  Role preference:   %s\n", report.RolePreference)

	boundStr := "no"
	if report.Bound {
		boundStr = "yes"
	}
	activatedStr := "no"
	if report.Activated {
		activatedStr = "yes"
	}
	fmt.Fprintf(w, "  Role bound:        %s\n", boundStr)
	fmt.Fprintf(w, "  Role activated:    %s\n", activatedStr)

	fmt.Fprintf(w, "\nPort Holders:\n")
	formatPortHolder(w, "TCP 443", report.Ports.TCP443)
	formatPortHolder(w, "UDP 443", report.Ports.UDP443)
	formatPortHolder(w, "UDP 4053", report.Ports.UDP4053)

	if report.Reading.Source != "" {
		fmt.Fprintf(w, "\nReading (%s):\n", report.Reading.Source)
		fmt.Fprintf(w, "  Role state:        %s\n", report.Reading.RoleState)
		if report.Reading.Ipv4 != "" {
			fmt.Fprintf(w, "  IPv4:              %s\n", report.Reading.Ipv4)
		}
		if report.Reading.Ipv6 != "" {
			fmt.Fprintf(w, "  IPv6:              %s\n", report.Reading.Ipv6)
		}
		if report.Reading.LastRefusal != "" && report.Reading.LastRefusal != "none" {
			fmt.Fprintf(w, "  Last refusal:      %s\n", report.Reading.LastRefusal)
		}
		if report.Reading.NextAttempt != "" {
			fmt.Fprintf(w, "  Next attempt:      %s\n", report.Reading.NextAttempt)
		}
	}

	if report.Firewall != nil {
		fmt.Fprintf(w, "\nFirewall (%s):\n", report.Firewall.Kind)
		fmt.Fprintf(w, "  %s\n", report.Firewall.Message)
	}

	if len(report.Warnings) > 0 {
		fmt.Fprintf(w, "\nWarnings:\n")
		for _, warn := range report.Warnings {
			fmt.Fprintf(w, "  - %s\n", warn)
		}
	}

	if len(report.Errors) > 0 {
		fmt.Fprintf(w, "\nIssues:\n")
		for _, errStr := range report.Errors {
			fmt.Fprintf(w, "  - %s\n", errStr)
		}
	}

	if report.Remedy != "" {
		fmt.Fprintf(w, "\nRemedy:\n  %s\n", report.Remedy)
	}

	return nil
}

func formatPortHolder(w io.Writer, label string, holder PortHolder) {
	if !holder.Held {
		fmt.Fprintf(w, "  %-10s free\n", label+":")
		return
	}
	proc := holder.Process
	if proc == "" && holder.PID > 0 {
		proc = fmt.Sprintf("PID %d", holder.PID)
	} else if proc != "" && holder.PID > 0 {
		proc = fmt.Sprintf("%s (PID %d)", proc, holder.PID)
	}
	if proc == "" {
		proc = "unknown process"
	}
	fmt.Fprintf(w, "  %-10s held by %s\n", label+":", proc)
}
