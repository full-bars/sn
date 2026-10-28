package urnettools

import (
	"os"
	"strings"
	"testing"
)

// Every place the status output names a configured source goes through the
// sanitizer. There are two surfaces: the dashboard's "Proxy Sources" block and
// the plain status panel's "URL sources" line.
func TestStatusProxySourcesAreSanitized(t *testing.T) {
	src, err := os.ReadFile("cobra.go")
	if err != nil {
		t.Fatalf("cannot read cobra.go (%v); this assertion needs the package dir as CWD", err)
	}
	if !strings.Contains(string(src), "sanitizeSourceForDisplay(src)") {
		t.Error("the proxy source line prints the stored source unsanitized")
	}

	panel, err := os.ReadFile("cli.go")
	if err != nil {
		t.Fatalf("cannot read cli.go (%v)", err)
	}
	if !strings.Contains(string(panel), "safe[i] = sanitizeSourceForDisplay(src)") {
		t.Error("the URL sources line does not go through the sanitizer")
	}
	if strings.Contains(string(panel), `strings.Join(urlSrc, ", ")`) {
		t.Error("the URL sources line still joins the raw sources")
	}
}

func TestSanitizeSourceForDisplay(t *testing.T) {
	const secret = "TOKEN123"
	cases := []struct{ in, want string }{
		// A token in the path is not printed: only scheme and host survive.
		{"https://api.example.com/lists/" + secret, "https://api.example.com"},
		{"https://user:pass@api.example.com/list?key=" + secret + "&x=1", "https://api.example.com"},
		{"https://api.example.com/list#token=" + secret, "https://api.example.com"},
		{"https://api.example.com/list", "https://api.example.com"},
		// The '@' in the query must not be read as the userinfo separator.
		{"user:pass@proxy:1080?key=alice@" + secret, "proxy:1080"},
		{"user:pass@proxy:1080", "proxy:1080"},
		{"proxy:1080?key=" + secret, "proxy:1080"},
	}
	for _, c := range cases {
		got := sanitizeSourceForDisplay(c.in)
		if strings.Contains(got, secret) || strings.Contains(got, "pass") || strings.Contains(got, "user:") {
			t.Errorf("sanitizeSourceForDisplay(%q) = %q: credential survived", c.in, got)
		}
		if got != c.want {
			t.Errorf("sanitizeSourceForDisplay(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Ambiguous input is withheld rather than guessed at.
	for _, in := range []string{"user:pass@", "user:pass@?key=" + secret, "   "} {
		if got := sanitizeSourceForDisplay(in); got != "<unparsed source>" {
			t.Errorf("sanitizeSourceForDisplay(%q) = %q, want the placeholder", in, got)
		}
	}
}
