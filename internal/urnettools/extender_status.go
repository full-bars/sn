package urnettools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

const (
	SourceControlSocket = "control socket"
	SourceUnknown       = "unavailable"
)

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
		LimitedBySourceCount: ex.LimitedBySourceCount,
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
}
