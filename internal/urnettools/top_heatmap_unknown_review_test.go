package urnettools

import (
	"strings"
	"testing"

	"github.com/urfoundation/sn/internal/tui"
)

func TestProxyCellInfoUnknownAndZeroInterval(t *testing.T) {
	m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
	m.proxyCells = makeProxyCells([]uint8{ProxyCellUp})
	if got := strings.Join(m.proxyCellInfo(0), "\n"); !strings.Contains(got, "bytes in interval ?") {
		t.Fatalf("missing detail: %s", got)
	}
	m.proxyDetail = map[int]ProxyCellDetail{0: {Index: 0, IntervalBytes: 0}}
	if got := strings.Join(m.proxyCellInfo(0), "\n"); !strings.Contains(got, "bytes in interval 0") {
		t.Fatalf("known zero: %s", got)
	}
}
