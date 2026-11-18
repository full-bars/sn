package urnettools

import (
	"fmt"
	"strings"
	"time"

	"github.com/urfoundation/sn/internal/tui"
)

type topEventView struct {
	At    time.Time
	Text  string
	Level topLevel
	Count int
}

func eventTime(at, now time.Time, zoom bool) string {
	if zoom {
		if !sameDay(at, now) {
			return at.Format("01-02 15:04")
		}
		return at.Format("15:04:05")
	}
	age := now.Sub(at)
	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(age.Hours()/24))
	}
}

func iconOr(r, fallback rune) rune {
	if r == 0 {
		return fallback
	}
	return r
}

func eventIcon(level topLevel, th tui.Theme) rune {
	if th.ASCII {
		switch level {
		case topInfo:
			return 'i'
		case topGood:
			return '+'
		case topWarn:
			return '!'
		case topBad:
			return 'x'
		}
		return '?'
	}
	switch level {
	case topInfo:
		return iconOr(th.IconOK, '•')
	case topGood:
		return '✓'
	case topWarn:
		return iconOr(th.IconWarn, '!')
	case topBad:
		return iconOr(th.IconBad, '✗')
	}
	return '?'
}

type eventFilter struct {
	MinLevel topLevel
	Text     string
}

func (f eventFilter) keep(e topEvent) bool {
	return e.Level >= f.MinLevel && (f.Text == "" || strings.Contains(strings.ToLower(e.Text), strings.ToLower(f.Text)))
}

func filterEvents(events []topEvent, f eventFilter) []topEvent {
	out := make([]topEvent, 0, len(events))
	for _, e := range events {
		if f.keep(e) {
			out = append(out, e)
		}
	}
	return out
}

// collapseEvents combines consecutive repetitions while retaining the newest time.
func collapseEvents(events []topEvent) []topEventView {
	if len(events) == 0 {
		return nil
	}
	out := make([]topEventView, 0, len(events))
	for _, e := range events {
		if n := len(out); n > 0 && out[n-1].Text == e.Text && out[n-1].Level == e.Level {
			out[n-1].Count++
			continue
		}
		out = append(out, topEventView{At: e.At, Text: e.Text, Level: e.Level, Count: 1})
	}
	return out
}

func (m *topModel) scrollEvents(delta int) {
	n := len(collapseEvents(filterEvents(m.sortedEvents(), m.eventFilter)))
	m.eventScroll = min(max(m.eventScroll+delta, 0), max(n-max(m.eventHeight, 1), 0))
}
