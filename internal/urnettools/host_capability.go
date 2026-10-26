package urnettools

import (
	"fmt"
	"time"
)

// HostCapability mirrors the provider-side record: what this host can reach,
// measured at startup and re-measured on a cadence. It is why an activation
// pass is charged one post or two, so it belongs in the status output where an
// operator can see it without reading logs.
type HostCapability struct {
	IPv6Usable           bool   `json:"ipv6_usable"`
	EverUsable           bool   `json:"ever_usable,omitempty"`
	CheckedAtUnix        int64  `json:"checked_at_unix,omitempty"`
	LastIPv6UsableAtUnix int64  `json:"last_ipv6_usable_at_unix,omitempty"`
	LastError            string `json:"last_error,omitempty"`
}

// formatHostCapability renders the line the status command prints.
func formatHostCapability(cap *HostCapability, now time.Time) string {
	if cap == nil {
		return "host capability: unknown (provider does not report it)"
	}
	checked := "never checked"
	if cap.CheckedAtUnix > 0 {
		checked = "checked " + humanSince(now.Sub(time.Unix(cap.CheckedAtUnix, 0)))
	}
	switch {
	case cap.IPv6Usable:
		return fmt.Sprintf("host capability: ipv4 + ipv6 (%s)", checked)
	case cap.EverUsable:
		last := ""
		if cap.LastIPv6UsableAtUnix > 0 {
			last = ", last usable " + humanSince(now.Sub(time.Unix(cap.LastIPv6UsableAtUnix, 0)))
		}
		return fmt.Sprintf("host capability: ipv4 only (%s%s)", checked, last)
	default:
		if cap.LastError != "" {
			return fmt.Sprintf("host capability: ipv4 only (%s; %s)", checked, cap.LastError)
		}
		return fmt.Sprintf("host capability: ipv4 only (%s; never had ipv6)", checked)
	}
}

// humanSince renders a duration the way an operator reads it.
func humanSince(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
