package urnettools

import (
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
)

// tailscaleCGNAT is the range Tailscale assigns node addresses from.
var tailscaleCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// metricsContainerMarker exists inside a Docker container. A var so tests
// can control it.
var metricsContainerMarker = "/.dockerenv"

// printMetricsStatus reports whether /metrics is on, every address it
// listens on, and the address a Prometheus on another machine should
// scrape, with a hint when there is none.
func printMetricsStatus(w io.Writer, resp controlResponse) {
	if len(resp.MetricsAddrs) == 0 {
		if s, ok := resp.Settings["metrics"]; ok && strings.EqualFold(s.Value, "on") {
			fmt.Fprintln(w, "Metrics: on, but the provider reported no listen address.")
			fmt.Fprintln(w, "Update the provider to 31.2 or later, or look for [metrics] errors in its log.")
			return
		}
		fmt.Fprintln(w, "Metrics: off")
		fmt.Fprintln(w, "Turn on with: urnet-tools metrics on")
		return
	}
	fmt.Fprintln(w, "Metrics: on")
	for _, addr := range resp.MetricsAddrs {
		fmt.Fprintf(w, "  listening  http://%s/metrics\n", addr)
	}
	target, hint := metricsScrapeTarget(resp.MetricsAddrs)
	if target != "" {
		fmt.Fprintf(w, "Prometheus target: %s\n", target)
	}
	if hint != "" {
		fmt.Fprintln(w, hint)
	}
}

// metricsScrapeTarget picks the address to give Prometheus: a Tailscale
// address first, then any other specific non-loopback address. Loopback
// and wildcard listeners have no single remote address, so they get a hint
// instead.
func metricsScrapeTarget(addrs []string) (target, hint string) {
	var parsed []netip.AddrPort
	for _, a := range addrs {
		if ap, err := netip.ParseAddrPort(a); err == nil {
			parsed = append(parsed, ap)
		}
	}
	for _, ap := range parsed {
		if tailscaleCGNAT.Contains(ap.Addr().Unmap()) {
			return ap.String(), ""
		}
	}
	for _, ap := range parsed {
		if !ap.Addr().IsLoopback() && !ap.Addr().IsUnspecified() {
			return ap.String(), ""
		}
	}
	for _, ap := range parsed {
		if !ap.Addr().IsUnspecified() {
			continue
		}
		if _, err := os.Stat(metricsContainerMarker); err == nil {
			return "", fmt.Sprintf("Listening on every interface inside this container. Publish the port when you run it,\n"+
				"for example -p <host Tailscale IP>:%d:%d, and use <host address>:%d as the Prometheus target.", ap.Port(), ap.Port(), ap.Port())
		}
		return "", fmt.Sprintf("Listening on every interface. Use this machine's address with port %d as the Prometheus target;\n"+
			"anyone who can reach that port can read these metrics.", ap.Port())
	}
	return "", "Loopback only, so Prometheus on another machine cannot reach it. Install Tailscale on this machine\n" +
		"(its address is picked up within 30 seconds), or choose an address: urnet-tools metrics listen <ip:port>"
}
