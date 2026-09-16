package provider

import (
	"fmt"
	"log"
	"os"
)

// Stubs for symbols referenced by Batch D+G files that are not yet present
// in the v2026 target workspace. These will be replaced when the upstream
// implementations are ported.

// tlog logs formatted output to stderr. In the fork this routes through a
// structured logger; here we fall back to fmt.Fprintf for now.
func tlog(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
}

// shmLogFatal logs a fatal message and exits with the given code. In the
// fork this writes to shared-memory log regions; here we use log.Fatalf.
func shmLogFatal(code int, format string, args ...any) {
	log.Fatalf("fatal [%d]: "+format, append([]any{code}, args...)...)
}

// DefaultConnectUrl is the default WebSocket connect endpoint.
const DefaultConnectUrl = "wss://connect.bringyour.com"

// defaultAPIHost is the default target for the API reachability probe.
const defaultAPIHost = "api.bringyour.com"

// defaultAPIPort is the default HTTPS port for the API reachability probe.
const defaultAPIPort = 443

// triggerProxyReload signals the running provider to reload its proxy
// configuration. In the fork this writes to a control socket; here it's
// a no-op stub until control_state integration is complete.
func triggerProxyReload() {
	// TODO: wire to control_state reload signal
}
