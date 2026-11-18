package urnettools

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/urfoundation/sn/internal/tui"
)

func TestTopProxyGridUnsupportedFallsBack(t *testing.T) {
	dir, err := os.MkdirTemp("", "grid")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	startAuditSocket(t, filepath.Join(dir, "provider.sock"), func(req controlRequest) string {
		if req.Cmd != "proxy_cells" {
			t.Errorf("command = %q", req.Cmd)
		}
		return `{"ok":false,"error":"unknown command: proxy_cells"}`
	})
	cells, err := fetchProxyCells(Provider{StateDir: dir})
	if !errors.Is(err, errTrafficUnsupported) || cells != nil {
		t.Fatalf("cells=%v err=%v", cells, err)
	}
	m := newTopModel(nil, 0, 0, tui.Theme{}, nil)
	m.proxyCells = []proxyCell{{index: 1, state: ProxyCellUp}}
	m.applyProxyCells(m.gen, cells, err)
	if len(m.proxyCells) != 0 || m.hasProxyGridData() {
		t.Fatal("unsupported reply retained stale grid data")
	}
}
