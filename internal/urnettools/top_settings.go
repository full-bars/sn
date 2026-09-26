package urnettools

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfoundation/sn/internal/tui"
)

// What the `top` menu remembers between runs: the color theme and the graph
// style. It lives in one small key=value file under the user's config
// directory. Reading is forgiving (a missing, unreadable or garbled file is
// just "no settings") because a display preference must never stop top from
// starting; writing reports its error so the menu can say the choice did not
// stick.

type topSettings struct {
	Theme string // a tui.ThemeNames entry, or "" for the environment's choice
	Graph string // a tui.GraphSymbolNames entry, or "" for braille
}

// topSettingsPath is ~/.config/urnet-tools/top.conf (or the platform's
// equivalent). Empty when there is no config directory, which turns
// persistence off.
func topSettingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "urnet-tools", "top.conf")
}

// loadTopSettings reads the file. Unknown keys and comments are skipped, and so
// is anything that does not name a real theme or style, so a stale or hand-
// edited file cannot select something that is not there.
func loadTopSettings(path string) topSettings {
	var s topSettings
	if path == "" {
		return s
	}
	f, err := os.Open(path)
	if err != nil {
		return s
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.Trim(strings.TrimSpace(val), `"`)
		switch key {
		case "theme":
			if _, ok := tui.ThemeByName(val); ok {
				s.Theme = val
			}
		case "graph":
			if _, ok := tui.GraphSymbolsByName(val); ok {
				s.Graph = val
			}
		}
	}
	return s
}

// ensureConfigDir creates the settings directory if it is missing and reports
// whether THIS call created it. The caller uses that to hand a directory it
// owns to the invoking user; an existing directory is never touched, so this
// cannot take over a directory the user created themselves.
func ensureConfigDir(dir string) (created bool, err error) {
	if _, err := os.Stat(dir); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	return true, nil
}

// saveTopSettings writes the file through a temporary file and a rename, so a
// crash mid-write never leaves half a file behind.
func saveTopSettings(path string, s topSettings) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	// MkdirAll leaves a root-owned directory behind when the first save runs
	// under sudo with HOME preserved: chownConfigFdToDirOwner then reads THAT
	// directory's owner, finds root, and changes nothing — so the user's next
	// non-sudo save cannot create a temp file in it. Hand a directory we just
	// created to the owner of the invoking user's home before anything is
	// written into it. Only when we created it, and best-effort.
	created, err := ensureConfigDir(dir)
	if err != nil {
		return err
	}
	if created {
		chownConfigDirToHomeOwner(dir)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".top.conf.*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	// An empty field means "leave whatever is already saved alone" — used when
	// the environment forces the theme. Because the save renames a whole new
	// file over the old one, that means carrying the current value forward, not
	// just skipping the line: skipping alone would drop every other saved
	// setting too.
	prev := loadTopSettings(path)
	if s.Theme == "" {
		s.Theme = prev.Theme
	}
	if s.Graph == "" {
		s.Graph = prev.Graph
	}
	var werr error
	if _, err := tmp.WriteString("# urnet-tools top: chosen in the m menu\n"); err != nil {
		werr = err
	}
	if werr == nil && s.Theme != "" {
		if _, err := fmt.Fprintf(tmp, "theme = %s\n", s.Theme); err != nil {
			werr = err
		}
	}
	if werr == nil {
		if _, err := fmt.Fprintf(tmp, "graph = %s\n", s.Graph); err != nil {
			werr = err
		}
	}
	if cerr := tmp.Chmod(0o644); werr == nil {
		werr = cerr
	}
	if werr != nil {
		tmp.Close()
		return werr
	}
	// Under sudo with HOME preserved, the config would land root-owned in the
	// invoking user's config dir, so that user's next non-sudo save cannot
	// overwrite it. When we are root, hand the temporary file to the owner of
	// its directory (usually the invoking user) while it is still an open fd,
	// before the rename, so a switcheroo to a symlink cannot redirect the
	// ownership change to some other root-owned file.
	chownConfigFdToDirOwner(tmp, filepath.Dir(path))
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return os.Rename(tmp.Name(), path)
}
