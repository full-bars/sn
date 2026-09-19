package urnettools

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stampOf builds "<prefix>v<ver>" at RUNTIME. These tests must not embed a
// complete stamp literal: other tests copy this test binary and scan it for a
// stamp, and a constant-folded literal here would be found first, unterminated.
func stampOf(ver string) string {
	return strings.Join([]string{versionStampPrefix, "v", ver}, "")
}

// scanVersionStamp must find a stamp whose prefix or value straddles a 64 KiB
// read boundary, and must never return a value cut off at a chunk edge.
func TestScanVersionStampAcrossReadBoundaries(t *testing.T) {
	const chunk = 64 * 1024
	ver := strings.Join([]string{"3", "23", "0-fix", "31", "2"}, ".")
	stamp := stampOf(ver) + "\x00"
	// Place the stamp so a chunk boundary falls at every offset inside it,
	// including inside the prefix and inside the value.
	for cut := 1; cut < len(stamp); cut++ {
		var b bytes.Buffer
		b.Write(bytes.Repeat([]byte{'x'}, chunk-cut))
		b.WriteString(stamp)
		b.Write(bytes.Repeat([]byte{'y'}, 1000))
		if got := scanVersionStamp(&b); got != "v"+ver {
			t.Fatalf("boundary at stamp offset %d: got %q", cut, got)
		}
	}
}

func TestScanVersionStampRules(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // version without the leading v ("" = no stamp)
	}{
		{"value at EOF without terminator", "junk" + stampOf("1.2.3"), "1.2.3"},
		{"v without digit skipped (adjacent rodata string)", versionStampPrefix + "vdata+EOF\x00" + stampOf("2.0.1") + "\x00", "2.0.1"},
		{"non-v value skipped, later stamp found", versionStampPrefix + "abc\x00" + stampOf("9.9.9") + " ", "9.9.9"},
		{"no stamp", "nothing here", ""},
		{"prefix at EOF with no value", "x" + versionStampPrefix, ""},
		{"lone v at EOF is not a version", "x" + versionStampPrefix + "v", ""},
		{"implausibly long value rejected", stampOf("9" + strings.Repeat("9", 400)), ""},
	}
	for _, c := range cases {
		want := ""
		if c.want != "" {
			want = "v" + c.want
		}
		if got := scanVersionStamp(strings.NewReader(c.in)); got != want {
			t.Errorf("%s: got %q, want %q", c.name, got, want)
		}
	}
	// A reader that returns data and io.EOF in the same call.
	if got := scanVersionStamp(iotestDataErrReader(stampOf("4.5.6"))); got != "v"+strings.Join([]string{"4", "5", "6"}, ".") {
		t.Errorf("data+EOF read: got %q", got)
	}
}

// iotestDataErrReader returns all data together with io.EOF on the first read.
type dataErrReader struct{ b []byte }

func iotestDataErrReader(s string) io.Reader { return &dataErrReader{b: []byte(s)} }

func (r *dataErrReader) Read(p []byte) (int, error) {
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, io.EOF
}

// Mixing a proxy file and a URL in one `proxy add` must be an error, not a
// silent skip of the file.
func TestProxyAddRejectsMixedFileAndURL(t *testing.T) {
	f := filepath.Join(t.TempDir(), "proxies.txt")
	if err := os.WriteFile(f, []byte("127.0.0.1:1080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := cmdProxy([]string{"add", f, "https://example.invalid/list.txt"}, false, true)
	if err == nil || !strings.Contains(err.Error(), "cannot mix") {
		t.Fatalf("mixed file+URL operands: got %v, want a 'cannot mix' error", err)
	}
}

// A missing state file must surface as ErrNotExist through the no-follow open
// so callers can treat "absent" as fine (Windows used to wrap the error and
// break errors.Is/os.IsNotExist for stageSessionFiles).
func TestOpenStateFileNoFollowMissingIsNotExist(t *testing.T) {
	_, err := openStateFileNoFollow(t.TempDir(), "does-not-exist")
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if !errors.Is(err, fs.ErrNotExist) || !os.IsNotExist(err) {
		t.Fatalf("missing file error must satisfy IsNotExist, got %v", err)
	}
}

// FetchSnStatus with no explicit StateDir must read the JWT from the resolved
// home state dir, never from a file in the current directory.
func TestFetchSnStatusUsesResolvedStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "jwt"), []byte("cwd-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)

	_, err := FetchSnStatus(Provider{})
	if err == nil {
		t.Fatal("expected a missing-JWT error")
	}
	want := filepath.Join(home, ".urnetwork", "jwt")
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error should reference the resolved state dir %q, got: %v", want, err)
	}
}

// On Windows `logs` must print its "not supported" notice and succeed even
// though a Windows provider has no systemd unit; elsewhere a unit-less
// provider is still refused with the actionable error.
func TestLogsPlatformCheck(t *testing.T) {
	noUnit := Provider{User: "someone", StateDir: "/x/.urnetwork"}
	handled, err := logsPlatformCheck(noUnit, "windows")
	if !handled || err != nil {
		t.Fatalf("windows: handled=%v err=%v, want handled=true err=nil", handled, err)
	}
	if handled, err := logsPlatformCheck(noUnit, "linux"); handled || err == nil {
		t.Fatalf("linux no-unit: handled=%v err=%v, want not handled and an error", handled, err)
	}
	withUnit := Provider{User: "someone", Unit: "urnetwork.service"}
	if handled, err := logsPlatformCheck(withUnit, "linux"); handled || err != nil {
		t.Fatalf("linux with unit: handled=%v err=%v, want continue", handled, err)
	}
}

// The root help must describe what `logs` actually accepts: a target and an
// optional line count. `all`, `dump` and `-i` were documented but never
// implemented, and cmdLogs rejects them as unexpected arguments.
func TestRootHelpLogsMatchesCommand(t *testing.T) {
	if strings.Contains(rootHelpMenu, "logs [all|dump|-i]") {
		t.Error("root help advertises logs modes (all|dump|-i) that cmdLogs does not implement")
	}
	if !helpMenuMentions("logs") || !strings.Contains(rootHelpMenu, "logs [target] [N]") {
		t.Error("root help must list `logs [target] [N]`, matching the registered command")
	}
	if err := cmdLogs([]string{"dump"}); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("cmdLogs(dump) = %v, want an unexpected-argument error", err)
	}
}

// readSessionLoadFile opens once and validates the OPENED file.
func TestReadSessionLoadFile(t *testing.T) {
	dir := t.TempDir()

	if _, err := readSessionLoadFile(filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing file: got %v, want a not-found error", err)
	}
	if _, err := readSessionLoadFile(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory: got %v, want a not-a-regular-file error", err)
	}
	f := filepath.Join(dir, "bundle.enc")
	if err := os.WriteFile(f, []byte("ciphertext"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := readSessionLoadFile(f)
	if err != nil || string(b) != "ciphertext" {
		t.Errorf("regular file: got %q, %v", b, err)
	}
}

// `session load <dir>` must be rejected before provider discovery and must
// not reach the passphrase prompt.
func TestSessionLoadRejectsDirectoryBeforeDiscovery(t *testing.T) {
	err := Run([]string{"session", "load", t.TempDir(), "-n"})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("session load <dir>: got %v, want a not-a-regular-file error", err)
	}
}
