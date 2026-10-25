//go:build linux

package miner

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRAMLogReaderDrainsDuringTrim holds a trim of one RAM log in progress and
// pushes far more than the 64 KiB pipe buffer through the ramlog pipe. The
// reader must keep draining (a blocked reader fills the pipe, and then every
// glog/tlog write in the process blocks behind it), and once the trim finishes
// every line written meanwhile must be in the file, in order, after the kept
// history. Run for the main log and for the important buffer, whose RAM
// fallback is trimmed the same way.
func TestRAMLogReaderDrainsDuringTrim(t *testing.T) {
	for _, blocked := range []string{"main", "important"} {
		t.Run(blocked, func(t *testing.T) { testRAMLogReaderDrainsDuringTrim(t, blocked) })
	}
}

func testRAMLogReaderDrainsDuringTrim(t *testing.T, blocked string) {
	const maxSize = 256 * 1024
	dir := t.TempDir()
	open := func(name string) *os.File {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0644)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	fMain, fImp := open("urnetwork.log"), open("important.log")
	// History to trim, in whichever file is blocked. Important lines carry a
	// marker so the other file stays empty of them.
	fill := fMain
	if blocked == "important" {
		fill = fImp
	}
	writeLines(t, fill, maxSize)
	mainLog, impLog := newRAMLogSink(fMain), newRAMLogSink(fImp)
	target := mainLog
	if blocked == "important" {
		target = impLog
	}

	started, release := make(chan struct{}), make(chan struct{})
	origTrim := ramLogTrim
	ramLogTrim = func(f *os.File, max, ratio int64) error {
		close(started)
		<-release
		return origTrim(f, max, ratio)
	}
	t.Cleanup(func() { ramLogTrim = origTrim })
	var released bool
	releaseTrim := func() {
		if !released {
			released = true
			close(release)
		}
	}
	t.Cleanup(releaseTrim)

	trimErr := make(chan error, 1)
	go func() { trimErr <- target.trim(maxSize, shmLogTrimRatio) }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("trim never started")
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pumped := make(chan struct{})
	go func() {
		pumpRAMLog(r, mainLog, impLog)
		r.Close()
		close(pumped)
	}()

	// 4096 lines of ~70 bytes is ~280 KiB, over four pipe buffers. On a reader
	// that waits for the trim the pipe fills and the write deadline fires.
	var sent bytes.Buffer
	for i := 0; i < 4096; i++ {
		fmt.Fprintf(&sent, "new %06d [profit] while the trim is in progress\n", i)
	}
	if err := w.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, werr := w.Write(sent.Bytes())
	if werr != nil {
		w.Close()
		releaseTrim()
		<-pumped
		if errors.Is(werr, os.ErrDeadlineExceeded) {
			t.Fatalf("pipe stopped draining while the %s log's trim was in progress: the reader waits on the trim", blocked)
		}
		t.Fatalf("write: %v", werr)
	}

	releaseTrim()
	if err := <-trimErr; err != nil {
		t.Fatalf("trim: %v", err)
	}
	w.Close()
	select {
	case <-pumped:
	case <-time.After(10 * time.Second):
		t.Fatal("reader did not finish after the pipe closed")
	}

	// Main log: every line, in order, after whatever history the trim kept.
	got, err := os.ReadFile(fMain.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(got, sent.Bytes()) {
		t.Fatalf("main log does not end with every pumped line in order (len %d, want suffix len %d)", len(got), sent.Len())
	}
	if blocked == "main" {
		history := got[:len(got)-sent.Len()]
		if int64(len(history)) > maxSize || len(history) == 0 {
			t.Fatalf("kept history is %d bytes, want 1..%d", len(history), maxSize)
		}
		if !strings.HasPrefix(string(history), "line ") || !bytes.HasSuffix(history, []byte("\n")) {
			t.Fatalf("kept history is not whole lines: %q...", history[:min(len(history), 80)])
		}
	}

	// Important buffer: every marked line, in order, after its kept history.
	gotImp, err := os.ReadFile(fImp.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(gotImp, sent.Bytes()) {
		t.Fatalf("important log does not end with every mirrored line in order (len %d)", len(gotImp))
	}
	if blocked == "important" {
		if hist := int64(len(gotImp) - sent.Len()); hist > maxSize || hist <= 0 {
			t.Fatalf("important kept history is %d bytes, want 1..%d", hist, maxSize)
		}
	}
}

// TestRAMLogSyncDoesNotTakeWriteLock: a Sync stuck in the kernel must not hold
// the lock the reader's writes take, so the sync must not need that lock at
// all. Holding it here stands in for a reader write in progress; the sync has
// to finish anyway.
func TestRAMLogSyncDoesNotTakeWriteLock(t *testing.T) {
	f, _ := openTestLog(t, os.O_RDWR)
	s := newRAMLogSink(f)
	s.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- s.sync() }()
	select {
	case err := <-done:
		s.mu.Unlock()
		if err != nil {
			t.Fatalf("sync: %v", err)
		}
	case <-time.After(5 * time.Second):
		s.mu.Unlock()
		<-done
		t.Fatal("sync waited for the write lock: a slow fsync would stall the pipe reader")
	}
}

// TestRAMLogRotateSwapsDescriptor: the on-disk important buffer rotates by
// rename; lines before the rotation are in path+".1", lines after in the new
// file at path, and a Sync on the swapped-in descriptor works.
func TestRAMLogRotateSwapsDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "important.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	s := newRAMLogSink(f)
	t.Cleanup(s.close)
	s.Write([]byte("before [profit] 1\n"))
	if err := s.rotate(path, 1); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	s.Write([]byte("after [profit] 2\n"))
	if err := s.sync(); err != nil {
		t.Fatalf("sync after rotate: %v", err)
	}
	if b, _ := os.ReadFile(path + ".1"); string(b) != "before [profit] 1\n" {
		t.Fatalf("%s.1 = %q", path, b)
	}
	if b, _ := os.ReadFile(path); string(b) != "after [profit] 2\n" {
		t.Fatalf("%s = %q", path, b)
	}
}
