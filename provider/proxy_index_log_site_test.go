package provider

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// The operator-visible proxy[N] label is the only way an operator correlates a
// log line with a specific proxy, and 3.23-fix took it from a field on its own
// ProxySettings struct that upstream connect does not have. sn resolves it from
// its own registry instead: setProxyIndex at launch, getProxyIndex at each log
// site.
//
// If a site is handed anything else, that proxy logs as proxy[0], which is the
// direct transport, or as some other proxy's number. Both are worse than a
// missing label: they are confidently wrong. So this pins every site to an
// index expression rather than trusting review to catch a stray proxySettings
// or a hardcoded literal.
//
// A source-reading test is the right tool here on purpose: the real path is a
// goroutine racing a live provider, and a test that needed that would be the
// flaky kind. This asserts the property that actually matters (every call site
// resolves through the registry) with no timing in it at all.
func TestProxyIndexLogSitesResolveThroughTheRegistry(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "provide.go", nil, 0)
	if err != nil {
		t.Fatalf("parse provide.go: %v", err)
	}

	// A call is a proxy[N] site when its first string argument contains
	// "proxy[%d]". The first non-literal argument after it is the index.
	const marker = "proxy[%d]"
	var sites, bad int

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		format, err := strconv.Unquote(lit.Value)
		if err != nil || !strings.Contains(format, marker) {
			return true
		}
		sites++

		// The index is the first argument after the format that is not itself
		// a string literal (the address and other %s fields are literals or
		// expressions; only the %d is a bare identifier or call).
		idx := -1
		for i, a := range call.Args[1:] {
			if b, ok := a.(*ast.BasicLit); ok && b.Kind == token.STRING {
				continue
			}
			idx = i
			break
		}
		if idx < 0 {
			bad++
			t.Errorf("provide.go:%d: proxy[N] call has no index argument: %s",
				fset.Position(call.Pos()).Line, truncateForMsg(format))
			return true
		}
		if !resolvesThroughRegistry(call.Args[1+idx]) {
			bad++
			t.Errorf("provide.go:%d: proxy[N] index is not a registry lookup, so it can "+
				"print the wrong proxy: %s", fset.Position(call.Pos()).Line, truncateForMsg(format))
		}
		return true
	})

	if sites == 0 {
		t.Fatal("no proxy[N] call sites found in provide.go; this test has stopped " +
			"describing the code and would pass on an empty file")
	}
	if bad > 0 {
		t.Fatalf("%d of %d proxy[N] call sites do not resolve their index through the "+
			"proxy registry", bad, sites)
	}
	t.Logf("checked %d proxy[N] call sites in provide.go", sites)
}

// resolvesThroughRegistry reports whether expr is getProxyIndex(...), the
// stableID local that getProxyIndex assigns, or a call to it. Anything else,
// including a literal 0, is rejected: a literal zero is the direct transport's
// index and would make every proxy look like it.
func resolvesThroughRegistry(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == "proxyIndex" || e.Name == "stableID"
	case *ast.CallExpr:
		if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "getProxyIndex" {
			return true
		}
		for _, a := range e.Args {
			if resolvesThroughRegistry(a) {
				return true
			}
		}
	}
	return false
}

func truncateForMsg(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 70 {
		return s[:70] + "..."
	}
	return s
}

// The direct transport holds index 0, so a registered proxy must never read
// back as 0. This is the property that makes a mis-wired site detectable at
// all: if setProxyIndex were given 0, or the key did not match, the lookup
// would silently return the direct transport's number.
func TestRegisteredProxyNeverReadsAsTheDirectIndex(t *testing.T) {
	const directIdx = 0
	key := (&connect.ProxySettings{
		Network: "tcp",
		Address: "198.51.100.7:1080",
		Auth:    &proxy.Auth{User: "carol", Password: "p"},
	}).Key()
	setProxyIndex(key, 4242)
	RegisterProxy(4242, "198.51.100.7:1080", key)
	t.Cleanup(func() {
		UnregisterProxy(4242)
		deleteProxyIndex(key)
	})

	if got := getProxyIndex(key); got == directIdx {
		t.Fatalf("a registered proxy reads as index %d, which is the direct transport's; "+
			"every such proxy would log as proxy[0]", got)
	}
	if got := ProxyKeyByIndex(4242); got != key {
		t.Errorf("index 4242 resolves to %q, want %q", got, key)
	}
	// prove the file the test parsed is the one it thinks it is
	if _, err := os.Stat(filepath.Join(".", "provide.go")); err != nil {
		t.Fatalf("stat provide.go: %v", err)
	}
}
