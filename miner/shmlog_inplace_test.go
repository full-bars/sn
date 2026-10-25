//go:build linux

package miner

// The in-place trim: the tail is read outside the writer's lock, the rewrite
// happens under it, and the file keeps its inode so descriptor-following
// readers keep working.

import (
	"io"
	"os"
	"strings"
	"testing"
)

func inPlaceTrimForTest(t *testing.T, f *os.File, maxSize int64, ratio int64, afterSnapshot func() error) bool {
	t.Helper()
	trimmed, err := trimRAMLogInPlace(
		f,
		maxSize,
		ratio,
		func(body func()) { body() },
		afterSnapshot,
	)
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	return trimmed
}

// TestTrimRAMLogInPlaceKeepsLinesAppendedDuringTheSnapshot is the defect the
// rename design had: while the tail is being read and copied, the reader keeps
// appending through the same descriptor. Those lines must survive the trim.
func TestTrimRAMLogInPlaceKeepsLinesAppendedDuringTheSnapshot(t *testing.T) {
	f, path := openTestLog(t, os.O_RDWR)
	writeLines(t, f, 64*1024)

	appended := "appended while the tail was being read\n"
	trimmed := inPlaceTrimForTest(t, f, 16*1024, 3, func() error {
		if _, err := f.WriteString(appended); err != nil {
			return err
		}
		return nil
	})
	if !trimmed {
		t.Fatal("an oversized log reported no trim")
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), appended) {
		t.Fatalf("the line written during the snapshot was lost; the log holds %d bytes", len(body))
	}
	if int64(len(body)) > 16*1024 {
		t.Fatalf("trim left %d bytes, over the %d cap", len(body), 16*1024)
	}
}

// TestTrimRAMLogInPlaceKeepsTheInode is the property `tail -f` and
// `provider logs` depend on: the path is rewritten in place, never replaced.
func TestTrimRAMLogInPlaceKeepsTheInode(t *testing.T) {
	f, path := openTestLog(t, os.O_RDWR)
	writeLines(t, f, 64*1024)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	inPlaceTrimForTest(t, f, 16*1024, 3, nil)
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("the trim replaced the file; a reader following the descriptor keeps the old inode and goes blind")
	}
}

// TestTrimRAMLogInPlaceKeepsAFollowerFollowing: a reader that opened the file
// before the trim still sees what is written after it.
func TestTrimRAMLogInPlaceKeepsAFollowerFollowing(t *testing.T) {
	f, path := openTestLog(t, os.O_RDWR)
	writeLines(t, f, 64*1024)

	follower, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer follower.Close()

	inPlaceTrimForTest(t, f, 16*1024, 3, nil)
	if _, err := f.WriteString("after the trim\n"); err != nil {
		t.Fatal(err)
	}

	body, err := io.ReadAll(follower)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "after the trim") {
		t.Fatal("a follower opened before the trim does not see lines written after it")
	}
}

// TestTrimRAMLogInPlaceUnderCapIsNoOp: nothing is rewritten and nothing is
// reported, so an idle log costs nothing every five seconds.
func TestTrimRAMLogInPlaceUnderCapIsNoOp(t *testing.T) {
	f, path := openTestLog(t, os.O_RDWR)
	f.WriteString("one line\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if inPlaceTrimForTest(t, f, 1024, 3, nil) {
		t.Fatal("a small log reported a trim")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("a no-op trim changed the file: %q -> %q", before, after)
	}
}

// TestTrimRAMLogInPlaceRejectsBadRatio keeps the guard the in-place trim has.
func TestTrimRAMLogInPlaceRejectsBadRatio(t *testing.T) {
	f, _ := openTestLog(t, os.O_RDWR)
	if _, err := trimRAMLogInPlace(f, 1024, 1, func(body func()) { body() }, nil); err == nil {
		t.Fatal("ratio 1 accepted; the kept share would be the whole file")
	}
}

// TestTrimRAMLogInPlaceLineBoundary: the kept tail starts on a full line.
func TestTrimRAMLogInPlaceLineBoundary(t *testing.T) {
	f, path := openTestLog(t, os.O_RDWR)
	writeLines(t, f, 64*1024)
	inPlaceTrimForTest(t, f, 16*1024, 3, nil)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 || body[0] == '\n' {
		t.Fatal("the trimmed log opens on an empty line; the partial first line must be dropped")
	}
	if !strings.HasSuffix(string(body), "\n") {
		t.Fatal("the trimmed log does not end on a line boundary")
	}
}
