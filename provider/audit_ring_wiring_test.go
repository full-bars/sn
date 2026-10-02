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

// reconcileAuditRingAfterHandoff is fully unit-tested in audit_ring_test.go,
// but a function nothing calls fixes nothing, which is how the last feature
// shipped inert. So prove by reading the source that the takeover path calls it
// with the provider's ctx and a wait that outlasts the parent's drain, after the
// takeover merge. The takeover lives in provideWithProxy here, not provide().
// Self-contained on purpose: it shares no helper with the baseline wiring test,
// which arrives in its own change.
func TestProvideReconcilesTheAuditRingAfterTakeover(t *testing.T) {
	text, err := os.ReadFile("provide.go")
	if err != nil {
		t.Fatalf("read provide.go: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "provide.go", text, 0)
	if err != nil {
		t.Fatalf("parse provide.go: %v", err)
	}

	var takeover *ast.FuncDecl
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "provideWithProxy" {
			takeover = fn
		}
	}
	if takeover == nil {
		t.Fatal("provideWithProxy() not found in provide.go")
	}

	mergeAt, reconcileAt := -1, -1
	var reconcileArgs []string
	ast.Inspect(takeover.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch auditWiringCalls(fset, call.Fun) {
		case "mergeAuditRingFromDisk":
			mergeAt = fset.Position(call.Pos()).Line
		case "reconcileAuditRingAfterHandoff":
			reconcileAt = fset.Position(call.Pos()).Line
			for _, a := range call.Args {
				reconcileArgs = append(reconcileArgs, auditWiringCalls(fset, a))
			}
		}
		return true
	})

	if mergeAt < 0 {
		t.Fatal("provideWithProxy() no longer calls mergeAuditRingFromDisk at takeover")
	}
	if reconcileAt < 0 {
		t.Fatal("provideWithProxy() never calls reconcileAuditRingAfterHandoff: the parent's drain-end persist can leave a parent-only ring on disk, and a crash soon after takeover loses the successor's start entry")
	}
	if reconcileAt <= mergeAt {
		t.Fatalf("reconcileAuditRingAfterHandoff at line %d is scheduled before the takeover merge at line %d, so it can re-read the ring before the merge that carries the parent's entries in", reconcileAt, mergeAt)
	}

	want := []string{"st.ctx", "HotSwapDrainTimeout + auditReconcileGrace"}
	if len(reconcileArgs) != len(want) {
		t.Fatalf("reconcileAuditRingAfterHandoff called with %d args, want %d: %v", len(reconcileArgs), len(want), reconcileArgs)
	}
	for i := range want {
		if reconcileArgs[i] != want[i] {
			if i == 0 {
				t.Errorf("reconcile is passed %q as its context, want the provider state ctx: a fresh context outlives shutdown and writes the file after main()'s final persist", reconcileArgs[i])
			} else {
				t.Errorf("reconcile waits %q, want HotSwapDrainTimeout+auditReconcileGrace: any shorter and it can run before the parent's drain-end persist and be overwritten by it", reconcileArgs[i])
			}
		}
	}
}

// auditWiringCalls renders a call's callee name, or an argument's source text.
// A call through a selector is reported by its bare method name, so a
// method-on-a-variable still matches the name under test. An argument that is
// itself a selector renders in full (st.ctx), because the receiver is the part
// that distinguishes one context from another.
func auditWiringCalls(fset *token.FileSet, n ast.Node) string {
	switch v := n.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if _, ok := v.X.(*ast.CallExpr); ok {
			// A call through a selector: report the method name alone.
			return v.Sel.Name
		}
		// A field selector: render it whole, the receiver included.
		var b strings.Builder
		if err := printer.Fprint(&b, fset, v); err != nil {
			return ""
		}
		return b.String()
	}
	var b strings.Builder
	if err := printer.Fprint(&b, fset, n); err != nil {
		return ""
	}
	return b.String()
}
