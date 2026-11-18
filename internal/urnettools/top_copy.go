package urnettools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfoundation/sn/internal/tui"
)

var boxToASCII = strings.NewReplacer(
	"─", "-", "│", "|",
	"┌", "+", "┐", "+", "└", "+", "┘", "+",
	"├", "+", "┤", "+", "┬", "+", "┴", "+", "┼", "+",
	"╭", "+", "╮", "+", "╰", "+", "╯", "+",
)

// copyText removes styles and optionally replaces box characters with ASCII.
func copyText(b *tui.Buffer, plain bool) string {
	s := b.String()
	if plain {
		return boxToASCII.Replace(s)
	}
	return s
}

func topStateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "urnet-tools")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "urnet-tools")
}

// writeTopCopy uses owner-only permissions because the screen can contain addresses.
func writeTopCopy(text string, now time.Time) (string, error) {
	dir := topStateDir()
	if dir == "" {
		return "", fmt.Errorf("no state dir to write the copy to")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "top-"+now.Format("20060102-150405")+"-*.txt")
	if err != nil {
		return "", err
	}
	path := f.Name()
	_, writeErr := f.WriteString(text)
	closeErr := f.Close()
	if writeErr != nil {
		return "", writeErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return path, nil
}

func (m *topModel) copyOnExit(b *tui.Buffer) {
	text := copyText(b, m.copyPlain)
	m.copyText = text
	path, err := writeTopCopy(text, m.now())
	if err != nil {
		m.copyPath, m.copyErr = "", err
		return
	}
	m.copyPath, m.copyErr = path, nil
}
