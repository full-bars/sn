package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProvideWiresThePrometheusPoolSnapshot fails if the provider stops
// registering its health registry with the connectx metrics seam.
//
// The seam is what makes the /metrics proxy-pool families appear at all: connectx
// deliberately owns no health state, and an unregistered seam makes the handler
// OMIT those families rather than print fabricated zeros. So dropping this one
// call does not fail the build and does not fail any handler test, it just
// silently stops reporting the pool. This test closes that hole by reading the
// source, so a refactor that moves or removes the call is a test failure rather
// than a quietly empty dashboard.
func TestProvideWiresThePrometheusPoolSnapshot(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("provide.go"))
	if err != nil {
		t.Fatalf("read provide.go: %v", err)
	}
	body := string(src)

	const call = "connectx.SetProxyPoolSnapshot(ProxyHealthSnapshot)"
	if !strings.Contains(body, call) {
		t.Fatalf("provide() must call %s at startup; without it the Prometheus "+
			"proxy-pool families are omitted and the pool is never reported", call)
	}

	// It must be inside provide() itself, not merely present in the file, or a
	// dead helper would satisfy the check above. Brace-match from the signature
	// to the function's own closing brace.
	i := strings.Index(body, "func provide(opts docopt.Opts) {")
	if i < 0 {
		t.Fatal("could not find the signature of provide() in provide.go")
	}
	depth, end := 0, -1
	for k := i; k < len(body); k++ {
		switch body[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				end = k
			}
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		t.Fatal("could not brace-match the body of provide()")
	}
	if !strings.Contains(body[i:end], call) {
		t.Errorf("%s must be called from provide() itself, so it runs on every "+
			"provider start; found it elsewhere in the file only", call)
	}
}
