package urnettools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExtenderSocketErrorMissingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.sock")
	dialErr := errors.New("dead network")
	if got := extenderSocketError(path, dialErr); !strings.Contains(got.Error(), "provider not running: no control socket") || !errors.Is(got, dialErr) {
		t.Fatalf("missing socket: %v", got)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got := extenderSocketError(path, dialErr); !strings.Contains(got.Error(), "control socket unavailable") || !errors.Is(got, dialErr) {
		t.Fatalf("existing path: %v", got)
	}
}
