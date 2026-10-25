package miner

// The composer and the publisher can both be perfect while nothing in
// production ever calls them: notifySystemdStatus sat with zero non-test
// callers before this port, so the line never appeared. These tests pin the
// wiring by reading the callers' own function bodies. A call parked in an
// uncalled helper does not count, because the match is made against the
// function body the runtime reaches.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// funcBodySource returns the source text of one function body, receiver
// qualified when recv is non-empty. Parsing the file (rather than scanning for
// a brace) makes the body end exactly where Go says it does.
func funcBodySource(t *testing.T, filename, name, recv string) string {
	t.Helper()
	src, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("read %s: %v", filename, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Body == nil {
			continue
		}
		if recv != "" {
			if fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			recvType := fn.Recv.List[0].Type
			if star, ok := recvType.(*ast.StarExpr); ok {
				recvType = star.X
			}
			ident, ok := recvType.(*ast.Ident)
			if !ok || ident.Name != recv {
				continue
			}
		} else if fn.Recv != nil {
			continue
		}
		start := fset.Position(fn.Body.Lbrace).Offset
		end := fset.Position(fn.Body.Rbrace).Offset
		if start < 0 || end <= start || end >= len(src) {
			t.Fatalf("%s: implausible body offsets for %s (%d..%d)", filename, name, start, end)
		}
		return string(src[start:end])
	}
	t.Fatalf("%s: no function %q (receiver %q) found", filename, name, recv)
	return ""
}

func requireCall(t *testing.T, filename, name, recv, call string) {
	t.Helper()
	body := funcBodySource(t, filename, name, recv)
	if !strings.Contains(body, call) {
		t.Errorf("%s %s does not call %s: the systemd STATUS line would never be refreshed from this path\nbody:\n%s",
			filename, name, call, body)
	}
}

// TestProxyStatusRefreshesOnTheAuditTick is the periodic delivery: the proxy
// audit loop already ticks forever, so the line rides it instead of starting a
// loop of its own.
func TestProxyStatusRefreshesOnTheAuditTick(t *testing.T) {
	requireCall(t, "proxy_manager_audit_run.go", "runProxyAudit", "", "refreshProxySystemdStatus()")
}

// TestProxyStatusPoolPublishedByManagerStart is the configured half: the line
// reports the launch set the manager just fixed, not a pool it guessed.
func TestProxyStatusPoolPublishedByManagerStart(t *testing.T) {
	requireCall(t, "proxy_manager.go", "start", "proxyManager", "setProxyStatusPool(launch)")
}

// TestProxyStatusRefreshesOnAuthTransitions is the immediate half: an auth
// success or failure changes the authenticated count, and v33 reported on
// those transitions rather than waiting for the next tick.
func TestProxyStatusRefreshesOnAuthTransitions(t *testing.T) {
	requireCall(t, "proxy_manager_auth.go", "noteProviderAuthSuccess", "", "markProxyStatusLive(")
	requireCall(t, "proxy_manager_auth.go", "noteProviderAuthFailure", "", "markProxyStatusDown(")
}

// TestProxyStatusRefreshesOnResolutionChange covers the zero-configured
// branches: resolution settles before a pool exists, and the line has to move
// with it.
func TestProxyStatusRefreshesOnResolutionChange(t *testing.T) {
	requireCall(t, "proxy_manager_hooks.go", "setProxyResolutionStatus", "", "reportProxyStatusToSystemd()")
}

// TestSystemdStatusComposerIsUsedByThePublisher keeps the composer from
// becoming dead code behind a second, divergent formatter: the publisher is
// the only place the line is rendered.
func TestSystemdStatusComposerIsUsedByThePublisher(t *testing.T) {
	requireCall(t, "systemd_status.go", "reportProxyStatusToSystemd", "", "notifySystemdStatus(systemdStatusLine())")
}

// TestProvideSignalsReady keeps READY=1 wired to the start of the provider run.
// The unit is Type=notify, so a provider that never sends it is killed by
// systemd at TimeoutStartSec while perfectly healthy - the cycle that reset
// every import attempt. It must NOT wait on proxy auth: auth can back off for
// hours, and the relay deliberately reports proxy health through STATUS=
// instead. notifySystemdReady already existed with no caller.
func TestProvideSignalsReady(t *testing.T) {
	requireCall(t, "run.go", "run", "providerRunSettings", "notifySystemdReady()")
}

// TestProvidePetsSystemdWatchdog keeps WATCHDOG=1 wired: a unit that sets
// WatchdogSec= kills a service that never pets it, whatever its health.
func TestProvidePetsSystemdWatchdog(t *testing.T) {
	requireCall(t, "run.go", "provideWith", "", "startSystemdWatchdog()")
}
