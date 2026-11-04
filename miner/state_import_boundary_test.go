package miner

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// urnet-tools must not open the provider's exclusively locked database.
func TestStateUrnetToolsDoesNotImportProviderStore(t *testing.T) {
	dir := filepath.Join("..", "internal", "urnettools")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			name, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if name == "go.etcd.io/bbolt" || name == "github.com/urfoundation/sn/miner" {
				t.Errorf("%s imports %s", path, name)
			}
		}
	}
}
