package urnettools

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/urfoundation/sn/internal/tui"
)

func TestTopCopyFlags(t *testing.T) {
	args := []string{"--copy", "--unit", "x", "--plain", "--interval=1s"}
	copy, plain, rest := stripTopCopyFlags(args)
	if !copy || !plain || !reflect.DeepEqual(rest, []string{"--unit", "x", "--interval=1s"}) || args[0] != "--copy" {
		t.Fatalf("flags: copy=%v plain=%v rest=%v args=%v", copy, plain, rest, args)
	}
}

func TestTopCopyTextAndPrivateFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := tui.New(20, 4)
	tui.DrawBox(b, "Top", tui.BorderRounded, tui.Style{}, tui.Style{}, false)
	b.Put("secret", 1, 1, tui.Style{})
	plain := copyText(b, true)
	if !strings.Contains(plain, "+") || strings.ContainsAny(plain, "╭─│") {
		t.Fatalf("plain copy: %q", plain)
	}
	now := time.Date(2026, 9, 19, 16, 4, 31, 0, time.UTC)
	path, err := writeTopCopy(plain, now)
	if err != nil {
		t.Fatal(err)
	}
	other, err := writeTopCopy(plain, now)
	if err != nil || other == path {
		t.Fatalf("second copy: %q %v", other, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != plain {
		t.Fatalf("copy content: %q %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v", info.Mode())
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && dir.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode: %v", dir.Mode())
	}
}

func TestTopCopyLoopAndWriteFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.copyOnQuit, m.copyPlain = true, true
	src, _ := fakeSource(t)
	rig := startLoop(t, m, src, 100, 30)
	rig.waitFor(t, "FLOWING", func(s string) bool { return strings.Contains(s, "FLOWING") })
	rig.sim.InjectKey(tcell.KeyRune, 'q', 0)
	if err := rig.wait(t); err != nil {
		t.Fatal(err)
	}
	if m.copyErr != nil || m.copyPath == "" || !strings.Contains(m.copyText, "FLOWING") {
		t.Fatalf("copy path=%q err=%v", m.copyPath, m.copyErr)
	}
	data, err := os.ReadFile(m.copyPath)
	if err != nil || string(data) != m.copyText {
		t.Fatalf("copy mismatch: %v", err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", file)
	b := tui.New(10, 2)
	b.Put("hello", 0, 0, tui.Style{})
	m.copyOnExit(b)
	if m.copyErr == nil || m.copyPath != "" || !strings.Contains(m.copyText, "hello") {
		t.Fatal("write failure lost screen text")
	}
}
