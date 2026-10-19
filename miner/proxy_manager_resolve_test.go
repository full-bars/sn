package miner

import (
	"fmt"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
)

// TestProxyManagerEnvOrFlagWarnings pins the v33 resolveDuration/resolveInt
// warning lines (provider/main.go): an unparseable flag or environment value
// names the source and the default it fell back to, instead of silently
// becoming the default.
func TestProxyManagerEnvOrFlagWarnings(t *testing.T) {
	defDur := time.Hour
	defInt := 100

	t.Run("duration flag", func(t *testing.T) {
		var got time.Duration
		out := captureTlog(t, func() {
			got = proxyManagerEnvOrFlagDuration(docopt.Opts{"--proxy_url_refresh": "bogus"}, "--proxy_url_refresh", "PROXY_URL_REFRESH", defDur)
		})
		if got != defDur {
			t.Fatalf("got %s, want the %s default", got, defDur)
		}
		want := fmt.Sprintf("[proxy][url] warning: invalid duration %q for %s; using default %s\n", "bogus", "--proxy_url_refresh", defDur)
		if out != want {
			t.Fatalf("line = %q, want %q", out, want)
		}
	})

	t.Run("duration env", func(t *testing.T) {
		t.Setenv("PROXY_URL_REFRESH", "bogus")
		var got time.Duration
		out := captureTlog(t, func() {
			got = proxyManagerEnvOrFlagDuration(docopt.Opts{}, "--proxy_url_refresh", "PROXY_URL_REFRESH", defDur)
		})
		if got != defDur {
			t.Fatalf("got %s, want the %s default", got, defDur)
		}
		want := fmt.Sprintf("[proxy][url] warning: invalid duration %q for %s; using default %s\n", "bogus", "PROXY_URL_REFRESH", defDur)
		if out != want {
			t.Fatalf("line = %q, want %q", out, want)
		}
	})

	t.Run("integer flag", func(t *testing.T) {
		var got int
		out := captureTlog(t, func() {
			got = proxyManagerEnvOrFlagInt(docopt.Opts{"--proxy_url_max": "bogus"}, "--proxy_url_max", "PROXY_URL_MAX", defInt)
		})
		if got != defInt {
			t.Fatalf("got %d, want the %d default", got, defInt)
		}
		want := fmt.Sprintf("[proxy][url] warning: invalid integer %q for %s; using default %d\n", "bogus", "--proxy_url_max", defInt)
		if out != want {
			t.Fatalf("line = %q, want %q", out, want)
		}
	})

	t.Run("integer env", func(t *testing.T) {
		t.Setenv("PROXY_URL_MAX", "bogus")
		var got int
		out := captureTlog(t, func() {
			got = proxyManagerEnvOrFlagInt(docopt.Opts{}, "--proxy_url_max", "PROXY_URL_MAX", defInt)
		})
		if got != defInt {
			t.Fatalf("got %d, want the %d default", got, defInt)
		}
		want := fmt.Sprintf("[proxy][url] warning: invalid integer %q for %s; using default %d\n", "bogus", "PROXY_URL_MAX", defInt)
		if out != want {
			t.Fatalf("line = %q, want %q", out, want)
		}
	})

	t.Run("valid values stay quiet", func(t *testing.T) {
		out := captureTlog(t, func() {
			if got := proxyManagerEnvOrFlagDuration(docopt.Opts{"--proxy_url_refresh": "30m"}, "--proxy_url_refresh", "PROXY_URL_REFRESH", defDur); got != 30*time.Minute {
				t.Fatalf("duration = %s, want 30m", got)
			}
			if got := proxyManagerEnvOrFlagInt(docopt.Opts{"--proxy_url_max": "7"}, "--proxy_url_max", "PROXY_URL_MAX", defInt); got != 7 {
				t.Fatalf("max = %d, want 7", got)
			}
		})
		if out != "" {
			t.Fatalf("valid values logged %q, want nothing", out)
		}
	})
}
