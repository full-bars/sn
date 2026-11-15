package urnettools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

const (
	SourceControlSocket = "control socket"
	SourceUnknown       = "unavailable"
)

// ExtenderCarrierBytes is the traffic this extender relayed on one carrier: bytes on
// each side since the process started, the rate at the last sample, the relayed
// connections open now and opened since start, and the last minutes of ingress plus
// egress as whole KiB. They are bytes this extender relayed, not earnings: the
// network credits contracts.
type ExtenderCarrierBytes struct {
	Carrier      string   `json:"carrier"`
	IngressBytes uint64   `json:"ingress_bytes"`
	EgressBytes  uint64   `json:"egress_bytes"`
	RateInBps    uint64   `json:"rate_in_bps,omitempty"`
	RateOutBps   uint64   `json:"rate_out_bps,omitempty"`
	LiveConns    uint64   `json:"live_conns"`
	ConnsTotal   uint64   `json:"conns_total"`
	Minutes      []uint32 `json:"minutes,omitempty"`
}

// ExtenderCarrierStats holds cumulative served and refused request counts for one carrier.
type ExtenderCarrierStats struct {
	Carrier         string            `json:"carrier"`
	Served          uint64            `json:"served"`
	Refused         uint64            `json:"refused"`
	RefusalsByStage map[string]uint64 `json:"refusals_by_stage,omitempty"`
}

// ExtenderReading holds the provider's live control-socket observation.
type ExtenderReading struct {
	Source                string
	RoleState             string
	Bound                 bool
	ActivatedV4           bool
	ActivatedV6           bool
	Ipv4                  string
	Ipv6                  string
	LastRefusal           string
	DnsPorts              string
	ConnectionCount       int
	PeerPingCount         int
	PeerPingCosignedCount int
	PeerPingRejectedCount int
	PeerPingUnknownCount  int
	LastPeerPingTime      int64
	LimitedBySubnetsCount int
	LimitedBySourceCount  int
	Carriers              []ExtenderCarrierStats
	CarrierBytes          []ExtenderCarrierBytes

	// BindRoute says how an unprivileged provider gets TCP 443 on this host.
	// It is a host reading set by the printing commands, not by the provider.
	BindRoute string
}

var (
	portRoutePlatformFn = func() string { return runtime.GOOS }
	portRouteReadFileFn = os.ReadFile
	portRouteExecCmdFn  = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}
)

// detectBindRoute reports the active route for TCP 443 on Linux; empty
// elsewhere.
func detectBindRoute(binary, user string) string {
	return detectBindRouteWith(portRoutePlatformFn(), binary, user, portRouteReadFileFn, portRouteExecCmdFn)
}

func detectBindRouteWith(platform, binary, user string, readFile func(string) ([]byte, error), execCmd func(string, ...string) ([]byte, error)) string {
	if platform != "linux" {
		return ""
	}
	if user == "root" {
		return "root (no capability or sysctl needed)"
	}
	if readFile != nil {
		if data, err := readFile(extenderSysctlPortStartPath); err == nil {
			if val, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && val >= 0 && val <= 443 {
				route := fmt.Sprintf("sysctl: port is available without a file capability (ip_unprivileged_port_start=%d)", val)
				if binary != "" && execCmd != nil {
					if out, err := execCmd("getcap", binary); err == nil && linuxHasBindCapability(binary, string(out)) {
						cmd := setupCommand{name: "sudo", args: []string{"setcap", "-r", binary}}.String()
						route += "; warning: binary still has a file capability, so a provider started with it is non-dumpable and may be unreadable to the updater. You must restart or hand off the provider after removing it. " + cmd
						if strings.TrimSpace(string(out)) != binary+" cap_net_bind_service=ep" {
							route += " removes ALL file capabilities; inspect getcap -n first and preserve other capabilities"
						}
					}
				}
				return route
			}
		}
	}
	if binary != "" && execCmd != nil {
		if out, err := execCmd("getcap", binary); err == nil && linuxHasBindCapability(binary, string(out)) {
			return "capability on the binary (cap_net_bind_service)"
		}
	}
	return "neither"
}

var socketExtenderFn = queryExtenderFromSocket

func queryExtenderFromSocket(p Provider) (*ExtenderReading, error) {
	if p.StateDir == "" {
		return nil, errors.New("no state directory")
	}
	sockPath := filepath.Join(p.StateDir, "provider.sock")
	resp, err := sendSocketRequest(sockPath, controlRequest{Cmd: "extender"})
	if err != nil {
		return nil, extenderSocketError(sockPath, err)
	}
	if !resp.OK || resp.Extender == nil || !resp.Extender.Known {
		return nil, errors.New("extender observation unavailable on control socket")
	}
	ex := resp.Extender
	roleState := "disabled"
	if ex.Enabled {
		switch {
		case ex.ActivatedV4 || ex.ActivatedV6:
			roleState = "active"
		case ex.Revoked:
			roleState = "revoked"
		case ex.ActivationRefused:
			roleState = "refused"
		case ex.ActivationFailed:
			roleState = "activation failed"
		case ex.ListenFailed:
			roleState = "listen failed"
		case ex.Listening:
			roleState = "listening"
		default:
			roleState = "enabled"
		}
	}
	return &ExtenderReading{
		Source: SourceControlSocket, RoleState: roleState, Bound: ex.Listening,
		ActivatedV4: ex.ActivatedV4, ActivatedV6: ex.ActivatedV6,
		Ipv4: ex.Ipv4, Ipv6: ex.Ipv6, LastRefusal: ex.LastActivationError,
		DnsPorts: ex.DnsPorts, ConnectionCount: ex.ConnectionCount,
		PeerPingCount: ex.PeerPingCount, PeerPingCosignedCount: ex.PeerPingCosignedCount,
		PeerPingRejectedCount: ex.PeerPingRejectedCount, PeerPingUnknownCount: ex.PeerPingUnknownCount,
		LastPeerPingTime: ex.LastPeerPingTime, LimitedBySubnetsCount: ex.LimitedBySubnetsCount,
		LimitedBySourceCount: ex.LimitedBySourceCount, Carriers: ex.Carriers, CarrierBytes: ex.CarrierBytes,
	}, nil
}

// Windows can report a network error when the Unix socket path does not exist.
func extenderSocketError(sockPath string, err error) error {
	_, statErr := os.Stat(sockPath)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("provider not running: no control socket: %w", err)
	}
	return fmt.Errorf("control socket unavailable: %w", err)
}

func ReadExtenderStatus(p Provider) ExtenderReading {
	if reading, err := socketExtenderFn(p); err == nil && reading != nil {
		return *reading
	}
	return ExtenderReading{Source: SourceUnknown, RoleState: "unavailable"}
}

func formatExtenderBlock(w io.Writer, reading ExtenderReading) {
	if reading.Source != SourceControlSocket {
		fmt.Fprintln(w, "\nExtender: unavailable")
		if reading.BindRoute != "" {
			fmt.Fprintf(w, "  Port route:        %s\n", reading.BindRoute)
		}
		return
	}
	fmt.Fprintf(w, "\nExtender:\n  Source:            %s\n  Role state:        %s\n", reading.Source, reading.RoleState)
	if reading.BindRoute != "" {
		fmt.Fprintf(w, "  Port route:        %s\n", reading.BindRoute)
	}
	fmt.Fprintf(w, "  Listening:         %t\n  IPv4 activated:    %t\n  IPv6 activated:    %t\n", reading.Bound, reading.ActivatedV4, reading.ActivatedV6)
	if reading.Ipv4 != "" {
		fmt.Fprintf(w, "  IPv4:              %s\n", reading.Ipv4)
	}
	if reading.Ipv6 != "" {
		fmt.Fprintf(w, "  IPv6:              %s\n", reading.Ipv6)
	}
	if reading.DnsPorts != "" {
		fmt.Fprintf(w, "  DNS ports:         %s\n", reading.DnsPorts)
	}
	if reading.LastRefusal != "" {
		fmt.Fprintf(w, "  Activation error:  %s\n", reading.LastRefusal)
	}
	fmt.Fprintf(w, "  Connections:       %d\n", reading.ConnectionCount)
	formatExtenderCarrierStats(w, reading.Carriers)
	formatExtenderTraffic(w, reading.CarrierBytes)
}

func formatExtenderCarrierStats(w io.Writer, carriers []ExtenderCarrierStats) {
	if len(carriers) == 0 {
		return
	}
	fmt.Fprintln(w, "  Carriers:")
	for _, c := range carriers {
		line := fmt.Sprintf("    %-9s served %d, refused %d", c.Carrier+":", c.Served, c.Refused)
		if len(c.RefusalsByStage) > 0 {
			stages := make([]string, 0, len(c.RefusalsByStage))
			for stage := range c.RefusalsByStage {
				stages = append(stages, stage)
			}
			slices.SortFunc(stages, func(a, b string) int {
				if c.RefusalsByStage[a] != c.RefusalsByStage[b] {
					if c.RefusalsByStage[a] > c.RefusalsByStage[b] {
						return -1
					}
					return 1
				}
				return strings.Compare(a, b)
			})
			parts := make([]string, 0, len(stages))
			for _, stage := range stages {
				parts = append(parts, fmt.Sprintf("%s %d", stage, c.RefusalsByStage[stage]))
			}
			line += " (" + strings.Join(parts, ", ") + ")"
		}
		fmt.Fprintln(w, line)
	}
}

// formatExtenderTraffic prints the traffic this extender relayed, per carrier, in
// human units. "in" is from clients and "out" is to clients.
func formatExtenderTraffic(w io.Writer, carriers []ExtenderCarrierBytes) {
	if len(carriers) == 0 {
		return
	}
	fmt.Fprintln(w, "  Traffic relayed (since the provider started):")
	fmt.Fprintf(w, "    %-9s %11s %11s %12s %12s  %s\n", "carrier", "in", "out", "rate in", "rate out", "connections")
	var in, out uint64
	for _, c := range carriers {
		in += c.IngressBytes
		out += c.EgressBytes
		fmt.Fprintf(w, "    %-9s %11s %11s %12s %12s  %d now, %d total\n", c.Carrier+":",
			formatBytes(float64(c.IngressBytes)), formatBytes(float64(c.EgressBytes)),
			formatBytes(float64(c.RateInBps))+"/s", formatBytes(float64(c.RateOutBps))+"/s", c.LiveConns, c.ConnsTotal)
	}
	fmt.Fprintf(w, "    %-9s %11s %11s\n", "all:", formatBytes(float64(in)), formatBytes(float64(out)))
}
