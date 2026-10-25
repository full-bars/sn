//go:build linux

package miner

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"time"
)

// mainTrimWarned and impTrimWarned track failure state so warnings repeat at
// a bounded rate rather than being silenced permanently.
var (
	mainTrimWarned   bool
	mainTrimWarnTime time.Time
	impTrimWarned    bool
	impTrimWarnTime  time.Time
)

// trimRAMLog bounds a RAM log file by discarding its oldest 1/ratio and
// keeping the newest (ratio-1)/ratio, in place, so the file descriptor and any
// `tail -f` following it stay valid.
//
// It fails closed. If the kept portion cannot be read back in full, the file is
// left exactly as it is: an oversized log that can still be read beats a
// correctly-sized log whose contents are gone. The original implementation did
// the opposite by accident. It read into a freshly allocated buffer, ignored
// the error, and wrote the buffer back regardless, so a failed read replaced
// the history with an equally long run of NUL bytes. Because the file lives on
// tmpfs, those NULs also went on occupying the RAM the cap exists to bound:
// the trim freed nothing and destroyed everything it claimed to preserve.
//
// f must be opened for reading as well as writing. That was the original
// defect: the log was opened O_WRONLY, so every ReadAt here returned EBADF.
func trimRAMLog(f *os.File, maxSize int64, ratio int64) error {
	if f == nil {
		return nil
	}
	if ratio < 2 {
		return fmt.Errorf("trim ratio %d must be at least 2", ratio)
	}

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if fi.Size() <= maxSize {
		return nil
	}

	keep := fi.Size() * (ratio - 1) / ratio
	if maxKeep := maxSize * (ratio - 1) / ratio; keep > maxKeep {
		keep = maxKeep
	}
	if keep <= 0 {
		return nil
	}

	b := make([]byte, keep)
	if _, err := f.ReadAt(b, fi.Size()-keep); err != nil {
		// Nothing is written: the oversized file survives intact.
		return fmt.Errorf("read newest %d bytes: %w", keep, err)
	}

	// The cut lands mid-line, so drop the partial first line. A log that opens
	// on half a line is confusing to read and breaks line-oriented tooling.
	if i := bytes.IndexByte(b, '\n'); i >= 0 && i+1 < len(b) {
		b = b[i+1:]
	}

	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("seek: %w", err)
	}
	// f.Seek after Truncate restores the position to 0, which is defense-in-
	// depth against a future regression that drops O_APPEND. With O_APPEND
	// every Write already goes to end-of-file, so the Seek is redundant in
	// normal operation but prevents a stray positioned write from clobbering
	// the kept tail.
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("write kept %d bytes: %w", len(b), err)
	}
	return nil
}

// reportTrimFailure writes a warning to the process's stderr, which is the
// RAM log itself once the redirect is in place. Unlike the original sync.Once
// approach, it re-emits after a 5-minute cooldown so a long-running daemon
// does not permanently silence the warning.
func reportTrimFailure(warned *bool, warnTime *time.Time, path string, err error) {
	now := time.Now()
	if *warned && now.Sub(*warnTime) < 5*time.Minute {
		return
	}
	*warned = true
	*warnTime = now
	fmt.Fprintf(os.Stderr, "[ramlogs] warning: cannot trim %s, leaving it oversized rather than losing history: %v\n", path, err)
}

// ramLogTail is the part of the log a trim keeps, read without the writer's
// lock. It carries the size it was snapshotted at so the locked step can pick
// up anything appended in between.
type ramLogTail struct {
	kept     []byte
	snapSize int64
	trimmed  bool
}

// readRAMLogTail reads the newest portion of the log, outside any lock.
//
// This is the expensive half of a trim: stat, a read of most of the cap, and a
// scan for the first newline. Doing it here rather than under the writer's lock
// is the point of the split: the goroutine draining the 64 KiB log pipe keeps
// writing while the tail is read, so the pipe cannot fill and no glog emit
// blocks behind the trim.
func readRAMLogTail(f *os.File, maxSize int64, ratio int64) (ramLogTail, error) {
	if f == nil {
		return ramLogTail{}, nil
	}
	if ratio < 2 {
		return ramLogTail{}, fmt.Errorf("trim ratio %d must be at least 2", ratio)
	}
	fi, err := f.Stat()
	if err != nil {
		return ramLogTail{}, fmt.Errorf("stat: %w", err)
	}
	if fi.Size() <= maxSize {
		return ramLogTail{}, nil
	}
	keep := fi.Size() * (ratio - 1) / ratio
	if maxKeep := maxSize * (ratio - 1) / ratio; keep > maxKeep {
		keep = maxKeep
	}
	if keep <= 0 {
		return ramLogTail{}, nil
	}

	b := make([]byte, keep)
	if _, err := f.ReadAt(b, fi.Size()-keep); err != nil {
		// Nothing is written: the oversized file survives intact.
		return ramLogTail{}, fmt.Errorf("read newest %d bytes: %w", keep, err)
	}
	// The cut lands mid-line, so drop the partial first line. A log that opens
	// on half a line is confusing to read and breaks line-oriented tooling.
	if i := bytes.IndexByte(b, '\n'); i >= 0 && i+1 < len(b) {
		b = b[i+1:]
	}
	return ramLogTail{kept: b, snapSize: fi.Size(), trimmed: true}, nil
}

// applyRAMLogTail rewrites the log in place from a snapshot. The caller must
// hold the writer's lock: the reader appends through the same descriptor, and
// the truncate must not fall between two of its writes.
//
// Anything appended after the snapshot is read back first and written with the
// kept tail in one write, so a trim cannot lose the lines produced while it was
// reading. The inode is never replaced, so `tail -f` and `provider logs`, which
// follow the descriptor, keep seeing the file.
func applyRAMLogTail(f *os.File, snap ramLogTail) error {
	if f == nil || !snap.trimmed {
		return nil
	}
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat before apply: %w", err)
	}
	var catchUp []byte
	if fi.Size() > snap.snapSize {
		catchUp = make([]byte, fi.Size()-snap.snapSize)
		n, readErr := f.ReadAt(catchUp, snap.snapSize)
		if readErr != nil && readErr != io.EOF {
			return fmt.Errorf("read catch-up: %w", readErr)
		}
		catchUp = catchUp[:n]
	}
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("seek: %w", err)
	}
	// One write, so a reader using the descriptor never observes a half-written
	// tail spliced onto the catch-up.
	body := append(append([]byte{}, snap.kept...), catchUp...)
	if _, err := f.Write(body); err != nil {
		return fmt.Errorf("write kept %d bytes: %w", len(body), err)
	}
	return nil
}

// trimRAMLogInPlace performs one trim: the snapshot outside the lock, then the
// rewrite under it.
//
// lock runs body while holding the writer's lock, and is passed in rather than
// taken here so a test can drive the same sequence without the real logger.
// afterSnapshot runs between the two, which is the window a test uses to append
// lines and prove none are lost.
func trimRAMLogInPlace(
	f *os.File,
	maxSize int64,
	ratio int64,
	lock func(func()),
	afterSnapshot func() error,
) (bool, error) {
	snap, err := readRAMLogTail(f, maxSize, ratio)
	if err != nil {
		return false, err
	}
	if !snap.trimmed {
		return false, nil
	}
	if afterSnapshot != nil {
		if err := afterSnapshot(); err != nil {
			return false, err
		}
	}
	var applyErr error
	lock(func() { applyErr = applyRAMLogTail(f, snap) })
	return true, applyErr
}
