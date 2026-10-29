//go:build !windows

package miner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

type chownCall struct {
	path     string
	uid, gid int
}

func stubChown(t *testing.T, fn func(string, int, int) error) *[]chownCall {
	t.Helper()
	var calls []chownCall
	prev := chownFile
	chownFile = func(path string, uid, gid int) error {
		calls = append(calls, chownCall{path, uid, gid})
		return fn(path, uid, gid)
	}
	t.Cleanup(func() { chownFile = prev })
	return &calls
}

func writeOwnerSource(t *testing.T) string {
	t.Helper()
	withTempHome(t)
	sourcePath := filepath.Join(t.TempDir(), "proxies.txt")
	if err := os.WriteFile(sourcePath, []byte("1.1.1.1:1080:alice:old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeProxyState(&ProxyState{Source: sourcePath, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	return sourcePath
}

// A rotation hands the replacement to the source's owner, before the rename.
// Without it a root-run `proxy add` leaves the service unable to read its file.
func TestProxyAddFileBackedKeepsSourceOwner(t *testing.T) {
	sourcePath := writeOwnerSource(t)
	info, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	calls := stubChown(t, func(string, int, int) error { return nil })

	proxyAdd(parseArgsForTest(t, []string{"proxy", "add", "1.1.1.1:1080:alice:new", "-f"}))

	if len(*calls) != 1 {
		t.Fatalf("chown calls = %d, want 1", len(*calls))
	}
	c := (*calls)[0]
	if c.uid != int(st.Uid) || c.gid != int(st.Gid) {
		t.Fatalf("chown to %d:%d, want source owner %d:%d", c.uid, c.gid, st.Uid, st.Gid)
	}
	if filepath.Dir(c.path) != filepath.Dir(sourcePath) || c.path == sourcePath {
		t.Fatalf("chown applied to %s, want the replacement beside the source", c.path)
	}
	if b, _ := os.ReadFile(sourcePath); !strings.Contains(string(b), ":alice:new") {
		t.Fatalf("rotation was not written:\n%s", b)
	}
}

// When the owner cannot be kept the source is left as it was and no temp file
// remains: no silent unreadable replacement.
func TestProxyAddFileBackedFailsWhenOwnerCannotBeKept(t *testing.T) {
	sourcePath := writeOwnerSource(t)
	stubChown(t, func(string, int, int) error { return errors.New("operation not permitted") })

	proxyAdd(parseArgsForTest(t, []string{"proxy", "add", "1.1.1.1:1080:alice:new", "-f"}))

	b, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "1.1.1.1:1080:alice:old\n" {
		t.Fatalf("source changed despite chown failure:\n%s", b)
	}
	entries, _ := os.ReadDir(filepath.Dir(sourcePath))
	for _, e := range entries {
		if e.Name() != "proxies.txt" {
			t.Fatalf("leftover file %s", e.Name())
		}
	}
}

func TestAtomicWriteFileOwnedReportsChownFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	stubChown(t, func(string, int, int) error { return errors.New("denied") })
	err := atomicWriteFileOwned(path, []byte("x"), 0600, &fileOwner{uid: 1, gid: 1})
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err = %v, want chown failure", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("file created despite failure: %v", statErr)
	}
}

// A new source has no owner to keep, so nothing is chowned.
func TestProxyAddFileBackedNewSourceIsNotChowned(t *testing.T) {
	withTempHome(t)
	sourcePath := filepath.Join(t.TempDir(), "new.txt")
	if err := writeProxyState(&ProxyState{Source: sourcePath, Proxies: map[string]ProxyEntry{}}); err != nil {
		t.Fatal(err)
	}
	calls := stubChown(t, func(string, int, int) error { return errors.New("must not be called") })

	proxyAdd(parseArgsForTest(t, []string{"proxy", "add", "1.1.1.1:1080:alice:pw", "-f"}))

	if len(*calls) != 0 {
		t.Fatalf("chown calls = %d, want 0", len(*calls))
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %04o, want 0600", info.Mode().Perm())
	}
}
