package urnettools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
		return
	}
	fmt.Fprintf(w, "\nExtender:\n  Source:            %s\n  Role state:        %s\n", reading.Source, reading.RoleState)
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
