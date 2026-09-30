package provider

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"strings"
	"testing"
)

// The self-management stack is inert without its startup wiring. Every one of
// these functions was ported, has its own test suite, and had NO non-test
// caller for a while: the tests call them directly and pass, while a real
// provider silently did none of them. That meant run.marker and oom_cap.json
// were never written, so the OOM cap never took effect in any mode,
// `urnet-tools set oom-cap on` reported success and changed nothing, the
// operator trim cap was not applied until a reload, and the resource-config
// startup warning never fired.
//
// So this reads provide.go and requires each call to exist inside
// provideLauncherLoop()'s own body, which is where sn runs the startup
// sequence, not merely somewhere in the package. A call parked in a helper
// nothing invokes does not satisfy it.
//
// It also pins the ORDER, because the order is the behaviour: the OOM cap must
// be decided before the trim selection it can override, and the launch count
// must be stored after the selection that decides how many proxies open.
func TestLauncherWiresTheStartupCapacityDecisions(t *testing.T) {
	body := launcherBody(t)

	required := []struct{ call, why string }{
		{"readOOMKillEpoch", "reads the boot id and OOM kill counter the cap decision needs"},
		{"oomCapDecide", "decides the OOM-aware start cap; without it the cap is inert"},
		{"startupTrimSelection", "applies the trim cap before the pool opens rather than shedding after"},
		{"primeTrimCapSeen", "stops the first reload re-reporting a cap startup already applied"},
		{"resourceConfigLaunchCount", "records how many proxies this start actually opens"},
		{"drainDeferredCrit", "flushes critical-log lines queued by the startup cap lookup"},
		{"ramCeilingLogLine", "tells the operator what RAM ceiling the process tunes against"},
		{"oomCapRecordStart", "records the launch for the NEXT start's decision"},
	}
	for _, r := range required {
		if body.firstIndex(r.call) < 0 {
			t.Errorf("provideLauncherLoop() never calls %s: %s. The ported function exists "+
				"and its own tests pass, so nothing fails without it, but the behaviour is dead.",
				r.call, r.why)
		}
	}

	// resourceConfigWarningsOnce must run from the first launch goroutine, after
	// the tier limits are applied, so it is checked in its own test below.
	mustPrecede := [][2]string{
		{"readOOMKillEpoch", "oomCapDecide"},
		{"oomCapDecide", "startupTrimSelection"},
		{"startupTrimSelection", "resourceConfigLaunchCount"},
		{"resourceConfigLaunchCount", "oomCapRecordStart"},
	}
	for _, pair := range mustPrecede {
		a, b := body.firstIndex(pair[0]), body.firstIndex(pair[1])
		if a < 0 || b < 0 {
			continue // already reported above
		}
		if a > b {
			t.Errorf("provideLauncherLoop() calls %s at line %d but %s first at line %d; the "+
				"startup decisions have to run in that order or the cap is computed from the "+
				"wrong state", pair[0], a, pair[1], b)
		}
	}

	// The scheduler must be handed the trimmed list. Passing allProxySettings
	// would launch every proxy and make the startup trim a no-op that only logs.
	if !body.hasCallWithFirstArg("prioritizeAndScheduleProxies", "launchSettings") {
		t.Error("provideLauncherLoop() does not schedule launchSettings: the startup trim cap " +
			"selects a smaller list, and scheduling allProxySettings would launch all of them " +
			"and make the cap cosmetic")
	}
}

// The reloader must be seeded with what actually launched, or its first reload
// sees no recorded auth for those proxies and rotates every one at boot.
func TestLauncherSeedsTheReloaderWithTheLaunchedList(t *testing.T) {
	if !launcherBody(t).hasCallWithFirstArg("seedRunningAuth", "launchSettings") {
		t.Error("provideLauncherLoop() does not seedRunningAuth(launchSettings): seeding the " +
			"full desired list makes the reloader rotate the proxies the startup trim held " +
			"back, relaunching them immediately and undoing the cap")
	}
}

// The configured count feeds the systemd status line, so it has to be the
// trimmed count or the box reports more proxies configured than it will run.
func TestLauncherReportsTheTrimmedConfiguredCount(t *testing.T) {
	if !launcherBody(t).hasCallWithFirstArg("setConfiguredProxyCount", "trimmedConfiguredCount(") {
		t.Error("provideLauncherLoop() does not set the trimmed configured count; the status " +
			"line would report the full desired count while a trim cap holds the pool lower")
	}
}

// The resource-config warning must run from the first launch goroutine, after the
// tier memory limits are applied. Read earlier it sees no limit at all on any
// auto, eco, turbo or default node, because the tier code sets GOMEMLIMIT after
// the startup block.
func TestFirstProxyGoroutineRunsTheResourceWarnings(t *testing.T) {
	body := funcBody(t, "provideWithProxy")
	warn := body.firstIndex("resourceConfigWarningsOnce")
	if warn < 0 {
		t.Error("provideWithProxy() never calls resourceConfigWarningsOnce: the startup memory " +
			"warning never fires, and the soft memory limit it reports is only in force after " +
			"the tier code has run")
		return
	}
	for _, before := range []string{"applyTurboMemoryLimit", "applyEcoSettings", "ensureMemoryLimit"} {
		if a := body.firstIndex(before); a >= 0 && a > warn {
			t.Errorf("resourceConfigWarningsOnce runs at line %d but %s at line %d; it must run "+
				"after the tier limits are applied or it reports a limit that is not in force",
				warn, before, a)
		}
	}
}

// startupWiring is a parsed function body with helpers to ask what it calls.
type startupWiring struct {
	fset  *token.FileSet
	block *ast.BlockStmt
	text  string
}

// hasCallWithFirstArg reports whether the named function is called at least once
// with want as the leading text of its first argument. It compares source text,
// so this is a check on the call site rather than on a reconstructed value.
func (p startupWiring) hasCallWithFirstArg(fn, want string) bool {
	found := false
	ast.Inspect(p.block, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeName(call.Fun) != fn || len(call.Args) == 0 {
			return true
		}
		if rendered := renderExpr(p.fset, call.Args[0]); strings.HasPrefix(rendered, want) {
			found = true
			return false
		}
		return true
	})
	return found
}

// firstIndex returns the 1-based line of the first call to name, or -1. Only
// real call expressions count, so a mention in a comment or string does not
// satisfy a requirement. The callee is matched by its last name component, so
// resourceConfigLaunchCount matches resourceConfigLaunchCount.Store(...) too.
func (p startupWiring) firstIndex(name string) int {
	found := -1
	ast.Inspect(p.block, func(n ast.Node) bool {
		if found >= 0 {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if calleeName(call.Fun) == name || p.calleeText(call.Fun, name) {
			found = p.fset.Position(call.Pos()).Line
			return false
		}
		return true
	})
	return found
}

// calleeText reports whether the callee's source text ends in "."+name, so
// resourceConfigLaunchCount.Store(...) counts as a call to
// resourceConfigLaunchCount, which is how a method on a package-level variable
// is wired.
// The receiver is the LEADING component, so resourceConfigLaunchCount.Store
// matches the name resourceConfigLaunchCount by prefix, not suffix.
func (p startupWiring) calleeText(fun ast.Expr, name string) bool {
	rendered := renderExpr(p.fset, fun)
	return strings.HasPrefix(rendered, name+".") || rendered == name
}

// renderExpr prints an expression back to Go source, so a test can compare the
// shape of an argument ("launchSettings", "trimmedConfiguredCount(...)") without
// depending on file offsets.
func renderExpr(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, e); err != nil {
		return ""
	}
	return b.String()
}

// calleeName is the identifier a call invokes, ignoring any receiver or package
// qualifier: r.F(...) and pkg.F(...) are both "F".
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

func launcherBody(t *testing.T) startupWiring {
	t.Helper()
	return funcBody(t, "provideLauncherLoop")
}

func funcBody(t *testing.T, name string) startupWiring {
	t.Helper()
	text, err := os.ReadFile("provide.go")
	if err != nil {
		t.Fatalf("read provide.go: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "provide.go", text, 0)
	if err != nil {
		t.Fatalf("parse provide.go: %v", err)
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Body == nil {
			continue
		}
		return startupWiring{fset: fset, block: fn.Body, text: string(text)}
	}
	t.Fatalf("%s() not found in provide.go", name)
	return startupWiring{}
}
