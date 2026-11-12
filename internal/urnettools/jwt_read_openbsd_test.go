package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadJWTFileOpenBSD(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jwt")
	if err := os.WriteFile(path, []byte("a.b.c"), 0600); err != nil {
		t.Fatal(err)
	}
	if b, err := readJWTFile(path); err != nil || string(b) != "a.b.c" {
		t.Fatalf("regular file: %q, %v", b, err)
	}
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readJWTFile(filepath.Join(link, "jwt")); err == nil {
		t.Fatal("symlinked directory accepted")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", maxJWTBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readJWTFile(path); err == nil {
		t.Fatal("oversized file accepted")
	}
	if _, err := readJWTFile(dir); err == nil {
		t.Fatal("directory accepted")
	}
}
