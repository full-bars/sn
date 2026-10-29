package miner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Rotating a file-backed proxy's password old -> new -> old leaves one stored
// line each time, and the credential the provider selects is the latest one.
// Appending beside the old line, and skipping the old password on the way back
// because its line was still present, leaves the first rotation's value live.
func TestProxyAddFileBackedRotatesPasswordAndBack(t *testing.T) {
	withTempHome(t)
	sourcePath := filepath.Join(t.TempDir(), "proxies.txt")
	if err := os.WriteFile(sourcePath, []byte("# keep\n1.1.1.1:1080:alice:old\n2.2.2.2:1080:bob:other\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{Source: sourcePath, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}

	for _, pw := range []string{"new", "old"} {
		proxyAdd(parseArgsForTest(t, []string{"proxy", "add", "1.1.1.1:1080:alice:" + pw, "-f"}))

		b, err := os.ReadFile(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		content := string(b)
		if got := strings.Count(content, "1.1.1.1:1080:alice:"); got != 1 {
			t.Fatalf("after rotating to %q the file holds %d entries for the account, want 1:\n%s", pw, got, content)
		}
		if !strings.Contains(content, "1.1.1.1:1080:alice:"+pw+"\n") {
			t.Fatalf("file does not hold the %q credential:\n%s", pw, content)
		}
		if !strings.Contains(content, "# keep") || !strings.Contains(content, "2.2.2.2:1080:bob:other") {
			t.Fatalf("rotation disturbed other lines:\n%s", content)
		}

		var selected []string
		for _, s := range readProxySettings() {
			if strings.HasPrefix(s.Address, "1.1.1.1") {
				selected = append(selected, s.Auth.Password)
			}
		}
		if len(selected) != 1 || selected[0] != pw {
			t.Fatalf("selected credentials for the account = %v, want just %q", selected, pw)
		}
	}
}

// A file that already carries two lines for one account (left by an earlier
// append) is collapsed to the requested credential, even when that exact line
// is one of them.
func TestProxyAddFileBackedCollapsesLegacyDuplicates(t *testing.T) {
	withTempHome(t)
	sourcePath := filepath.Join(t.TempDir(), "proxies.txt")
	if err := os.WriteFile(sourcePath, []byte("1.1.1.1:1080:alice:old\n1.1.1.1:1080:alice:new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{Source: sourcePath, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	proxyAdd(parseArgsForTest(t, []string{"proxy", "add", "1.1.1.1:1080:alice:old", "-f"}))
	b, _ := os.ReadFile(sourcePath)
	if string(b) != "1.1.1.1:1080:alice:old\n" {
		t.Fatalf("file = %q, want one line with the requested credential", b)
	}
}
