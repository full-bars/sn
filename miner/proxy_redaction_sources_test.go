package miner

import (
	"os"
	"strings"
	"testing"
)

// A proxy source line the parser cannot split must not be echoed: the parser
// returns the whole line as the address when the port is not numeric, so a
// password would be printed beside "password=absent".
func TestRedactProxySourceWithholdsUnparsedUserinfo(t *testing.T) {
	const password = "s3cr3tpass"
	lines := []string{
		"alice:" + password + "@proxy:bad",
		"socks5://alice:" + password + "@proxy:bad",
		"alice:" + password + "@proxy",
		"alice:" + password + "@",
		"alice:" + password + "@1.2.3.4:notaport",
	}
	for _, line := range lines {
		out := redactProxySource(line)
		if strings.Contains(out, password) {
			t.Errorf("redactProxySource(%q) = %q: password survived", line, out)
		}
		if strings.Contains(out, "alice") {
			t.Errorf("redactProxySource(%q) = %q: user survived", line, out)
		}
		// Two acceptable reports: the line could not be split at all, or the
		// userinfo was split off and reported as present with no address.
		// What must never happen is credentials in the text beside "absent".
		if !strings.Contains(out, "unparsed") && !strings.Contains(out, "user=present") {
			t.Errorf("redactProxySource(%q) = %q: a line carrying userinfo is reported as credential-free", line, out)
		}
	}

	// A well-formed line keeps its diagnostic value: host, port and the two
	// presence labels, with no credential bytes.
	if got := redactProxySource("alice:" + password + "@1.2.3.4:1080"); got != "1.2.3.4:1080 (user=present, password=present)" {
		t.Errorf("well-formed redaction = %q", got)
	}
	if got := redactProxySource("proxy.example:1080"); got != "proxy.example:1080 (user=absent, password=absent)" {
		t.Errorf("credential-free redaction = %q", got)
	}
	// A scheme that itself carries an '@' is dropped rather than echoed.
	if got := redactProxySource("alice:pw@x://proxy:1080"); strings.Contains(got, "pw") {
		t.Errorf("scheme path leaked: %q", got)
	}
}

// The source-management commands print the source they acted on. A source URL
// can carry userinfo and query tokens, so every one of those lines must go
// through the sanitizer rather than printing the stored string.
func TestSourceCommandOutputIsSanitized(t *testing.T) {
	src, err := os.ReadFile("proxy_manager_commands.go")
	if err != nil {
		t.Fatalf("cannot read proxy_manager_commands.go (%v); this assertion needs the package dir as CWD", err)
	}
	body := string(src)
	if n := strings.Count(body, "sanitizeURLForDisplay(url)"); n != 4 {
		t.Errorf("sanitized source lines = %d, want 4 (added, already added, not found, removed)", n)
	}
	for _, raw := range []string{
		`already added: %s\n", url)`,
		`added source: %s\nfetching now...\n", url)`,
		`source not found: %s\n", url)`,
		`removed source: %s\n", url)`,
	} {
		if strings.Contains(body, raw) {
			t.Errorf("unsanitized source URL printed: %s", raw)
		}
	}
}
