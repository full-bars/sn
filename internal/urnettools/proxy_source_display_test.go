package urnettools

import (
	"os"
	"strings"
	"testing"
)

// The status page lists configured proxy sources. A source can carry userinfo
// or an API token in its query string, so neither may reach the screen.
func TestStatusProxySourcesAreSanitized(t *testing.T) {
	src, err := os.ReadFile("cobra.go")
	if err != nil {
		t.Fatalf("cannot read cobra.go (%v); this assertion needs the package dir as CWD", err)
	}
	if !strings.Contains(string(src), "sanitizeSourceForDisplay(src)") {
		t.Error("the proxy source line prints the stored source unsanitized")
	}
}

func TestSanitizeSourceForDisplay(t *testing.T) {
	const secret = "TOKEN123"
	cases := []struct{ in, want string }{
		{"https://user:pass@api.example.com/list?key=" + secret + "&x=1", "https://api.example.com/list"},
		{"https://api.example.com/list#token=" + secret, "https://api.example.com/list"},
		{"https://api.example.com/list", "https://api.example.com/list"},
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
	if got := sanitizeSourceForDisplay("user:pass@"); got != "<unparsed source>" {
		t.Errorf("bare userinfo source = %q, want the placeholder", got)
	}
}
