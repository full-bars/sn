//go:build linux

package miner

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestImportantLogFollow(t *testing.T) {
	for _, action := range []string{"replace", "remove", "symlink", "permissions"} {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "important.log")
			f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { f.Close() }()
			var mu sync.Mutex
			if changed, err := followImportantLogPath(path, &f, &mu); err != nil || changed {
				t.Fatalf("unchanged: %v %v", changed, err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "replace":
				if err := os.WriteFile(path, []byte("other\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(dir, "target"), path); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.WriteFile(path, nil, 0644); err != nil {
					t.Fatal(err)
				}
			}
			changed, err := followImportantLogPath(path, &f, &mu)
			if action == "symlink" || action == "permissions" {
				if err == nil || changed {
					t.Fatalf("unsafe path accepted: %v %v", changed, err)
				}
				return
			}
			if err != nil || !changed {
				t.Fatalf("follow: %v %v", changed, err)
			}
			if _, err := f.WriteString("new\n"); err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(path)
			if err != nil || len(contents) < 4 || string(contents[len(contents)-4:]) != "new\n" {
				t.Fatalf("content %q: %v", contents, err)
			}
		})
	}
}
