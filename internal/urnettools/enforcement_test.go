package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseEnforcementArgs(t *testing.T) {
	for _, c := range []struct {
		args  []string
		sub   string
		limit int
		err   string
	}{
		{nil, "summary", enforcementDefaultLimit, ""},
		{[]string{"summary"}, "summary", enforcementDefaultLimit, ""},
		{[]string{"history"}, "history", enforcementDefaultLimit, ""},
		{[]string{"history", "50"}, "history", 50, ""},
		{[]string{"log", "5"}, "history", 5, ""},
		{[]string{"history", "99999"}, "history", enforcementMaxLimit, ""},
		{[]string{"history", "0"}, "", 0, "positive integer"},
		{[]string{"history", "x"}, "", 0, "positive integer"},
		{[]string{"summary", "extra"}, "", 0, "no arguments"},
		{[]string{"bogus"}, "", 0, "unknown subcommand"},
	} {
		sub, limit, _, err := parseEnforcementArgs(c.args)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v: err = %v, want %q", c.args, err, c.err)
			}
			continue
		}
		if err != nil || sub != c.sub || limit != c.limit {
			t.Errorf("%v: got %q %d %v, want %q %d", c.args, sub, limit, err, c.sub, c.limit)
		}
	}
}

func TestRenderEnforcementSummaryWindows(t *testing.T) {
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	d := enforcementDaily{
		Total: map[string]int{"dead_client_release|enforce": 9, "budget_cut|shadow": 2},
		Days: map[string]map[string]int{
			"2026-10-12": {"dead_client_release|enforce": 1},
			"2026-10-08": {"dead_client_release|enforce": 3}, // 4 days back: in 7d
			"2026-09-25": {"dead_client_release|enforce": 4}, // 17 days back: in 30d only
			"2026-10-11": {"budget_cut|shadow": 2},
		},
	}
	out := renderEnforcementSummary(d, now)
	want := map[string][]string{
		"dead_client_release": {"enforce", "1", "4", "8", "9"},
		"budget_cut":          {"shadow", "0", "2", "2", "2"},
	}
	for kind, cols := range want {
		var line string
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, kind) {
				line = l
			}
		}
		if got := strings.Fields(line); len(got) != 6 || strings.Join(got[1:], " ") != strings.Join(cols, " ") {
			t.Errorf("%s row = %q, want columns %v", kind, line, cols)
		}
	}
	if !strings.Contains(out, "only 'enforce' rows changed behaviour") {
		t.Errorf("missing shadow explanation:\n%s", out)
	}
	if got := renderEnforcementSummary(enforcementDaily{}, now); !strings.Contains(got, "No enforcement events") {
		t.Errorf("empty = %q", got)
	}
}

func TestReadEnforcementTailSpansRotationAndSkipsJunk(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(enforcementLogFile+".1", `{"ts":"2026-10-12T10:00:00Z","kind":"a","mode":"enforce","line":"[govern] one"}`+"\n")
	write(enforcementLogFile, "not json\n"+
		`{"ts":"2026-10-12T11:00:00Z","kind":"b","mode":"shadow","line":"[govern] two"}`+"\n"+
		`{"ts":"2026-10-12T12:00:00Z","kind":"c","mode":"enforce","line":"[govern] three"}`+"\n")

	rows, err := readEnforcementTail(dir, 10)
	if err != nil || len(rows) != 3 || rows[0].Kind != "a" || rows[2].Kind != "c" {
		t.Fatalf("rows = %+v err = %v", rows, err)
	}
	rows, _ = readEnforcementTail(dir, 2)
	if len(rows) != 2 || rows[0].Kind != "b" {
		t.Errorf("limit kept the wrong rows: %+v", rows)
	}
	out := renderEnforcementHistory(rows)
	if !strings.Contains(out, "2026-10-12 11:00:00Z") || !strings.Contains(out, "two") || strings.Contains(out, "[govern]") {
		t.Errorf("history output:\n%s", out)
	}
}

func TestReadEnforcementMissingFilesAreEmptyNotErrors(t *testing.T) {
	dir := t.TempDir()
	if d, err := readEnforcementDaily(dir); err != nil || len(d.Total) != 0 {
		t.Errorf("daily = %+v %v", d, err)
	}
	if rows, err := readEnforcementTail(dir, 5); err != nil || len(rows) != 0 {
		t.Errorf("tail = %+v %v", rows, err)
	}
	os.WriteFile(filepath.Join(dir, enforcementDailyFile), []byte("{bad"), 0600)
	if _, err := readEnforcementDaily(dir); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("a corrupt file must be reported, got %v", err)
	}
}

func TestEnforcementAbsentNoteOnlyWhenNoLogFilesExist(t *testing.T) {
	dir := t.TempDir()
	note := enforcementAbsentNote(dir)
	if !strings.Contains(note, "No enforcement log found in "+dir) ||
		!strings.Contains(note, "older than the version that records it") {
		t.Fatalf("absent dir note = %q", note)
	}
	// Either file existing, even empty, means there is a log to show: the
	// plain "no events yet" message must stay.
	for _, name := range []string{enforcementLogFile, enforcementDailyFile} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
		if got := enforcementAbsentNote(dir); got != "" {
			t.Errorf("%s exists but note = %q", name, got)
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, enforcementLogFile), nil, 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := readEnforcementTail(dir, enforcementDefaultLimit)
	if err != nil || renderEnforcementHistory(rows) != "No enforcement events recorded yet.\n" {
		t.Errorf("empty existing log: rows = %v err = %v", rows, err)
	}
}
