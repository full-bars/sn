//go:build linux

package miner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
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

// ramLogTrim is trimRAMLog behind a seam, so a test can hold a trim in
// progress and watch what the pipe reader does meanwhile.
var ramLogTrim = trimRAMLog

// ramLogSink is one RAM log file (the main log or the important buffer).
//
// The pipe reader (pumpRAMLog) is the only writer, and every glog/tlog write
// in the process waits on it once the 64 KiB pipe buffer is full. So the
// reader must never wait on the slow work: the trim (an in-place rewrite of up
// to ~3.3 MiB of the 5 MiB main log every 5 s), the 500 ms Sync, or the
// important buffer's rotation. mu covers exactly one reader write or one cheap
// state change, nothing longer:
//
//   - trim sets trimming under mu and rewrites the file without it. Writes
//     that arrive meanwhile are appended to pending instead of the file; the
//     trim flushes pending in one write under mu, then clears trimming. Lines
//     are neither lost nor reordered: everything written before trimming was
//     set is in the file the trim reads, everything after lands behind the kept
//     tail in arrival order.
//   - sync calls Sync on a snapshot of the descriptor and takes no lock.
//   - rotate (the on-disk important buffer) renames and opens a file, then
//     swaps the descriptor under mu.
//
// The trim stays in place, on the same inode, rather than writing a new file
// and renaming it over the old one. Everything that follows the log follows
// the descriptor: the `tail -f` hint printed at startup, `docker exec ... tail
// -n N -f` in urnet-tools (internal/urnettools/cli_docker.go cmdDockerLogs),
// docker/scripts/logs.sh, scripts/Provider_Install_Linux.sh, and the miner's
// own `logs` command (cmd_logs.go providerLogs, which reopens only when the
// descriptor's size shrinks behind its position). A rename would leave every
// one of them reading the unlinked old inode, silent, after the first trim.
// Truncation they already handle.
type ramLogSink struct {
	f atomic.Pointer[os.File] // swapped only under mu (rotate); read freely

	mu       sync.Mutex
	trimming bool   // a trim is rewriting the file: writes go to pending
	pending  []byte // writes held back during a trim, in arrival order

	slow sync.Mutex // serializes trim and rotate with each other
}

func newRAMLogSink(f *os.File) *ramLogSink {
	s := &ramLogSink{}
	s.f.Store(f)
	return s
}

// Write appends p, or queues it behind a trim in progress. It is the only call
// the pipe reader makes, and it holds mu for one write at most.
func (s *ramLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.trimming {
		s.pending = append(s.pending, p...)
		return len(p), nil
	}
	return s.f.Load().Write(p)
}

// trim bounds the file in place (see trimRAMLog) without holding mu for the
// rewrite.
func (s *ramLogSink) trim(maxSize, ratio int64) error {
	s.slow.Lock()
	defer s.slow.Unlock()
	f := s.f.Load()
	if fi, err := f.Stat(); err == nil && fi.Size() <= maxSize {
		return nil // the common case: no reason to divert the writes
	}

	s.mu.Lock()
	s.trimming = true
	s.mu.Unlock()

	err := ramLogTrim(f, maxSize, ratio)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.trimming = false
	if len(s.pending) > 0 {
		_, werr := f.Write(s.pending)
		s.pending = nil // release the memory; trims are seconds apart
		if werr != nil {
			return errors.Join(err, fmt.Errorf("write lines held during trim: %w", werr))
		}
	}
	return err
}

// sync flushes the file so a follower sees new lines promptly. It takes no
// lock: an fsync that stalls in the kernel must not stall the reader. If a
// rotation closes the snapshot first, Sync returns an error and the next tick
// syncs the new file.
func (s *ramLogSink) sync() error {
	return s.f.Load().Sync()
}

// rotate moves the file to path+".1" once it passes max (see
// rotateImportantLog) and swaps in the fresh one. Only the swap holds mu. A
// reader write that lands on the old descriptor between the rename and the
// swap goes to path+".1", ahead of everything written to the new file, so
// order holds across the two files.
func (s *ramLogSink) rotate(path string, max int64) error {
	s.slow.Lock()
	defer s.slow.Unlock()
	cur := s.f.Load()
	next, err := rotateImportantLog(cur, path, max)
	if err != nil || next == nil {
		return err
	}
	s.mu.Lock()
	s.f.Store(next)
	s.mu.Unlock()
	cur.Close()
	return nil
}

func (s *ramLogSink) close() {
	s.slow.Lock()
	defer s.slow.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.Load().Close()
}

// pumpRAMLog drains the ramlog pipe into main and mirrors complete important
// lines into imp (nil disables the mirror) until r fails. Both tlog and glog
// output flow through this pipe, so this captures [profit]/[earn]/[health]/etc.
// regardless of which logger emits. Every glog/tlog write in the process waits
// on this loop once the 64 KiB pipe buffer is full, so it must never wait on
// anything slow.
func pumpRAMLog(r io.Reader, main, imp *ramLogSink) {
	buf := make([]byte, 32*1024)
	var lineBuf []byte // accumulates a partial trailing line across reads
	for {
		n, err := r.Read(buf)
		if n > 0 {
			main.Write(buf[:n])
			if imp != nil {
				lineBuf = append(lineBuf, buf[:n]...)
				for {
					i := bytes.IndexByte(lineBuf, '\n')
					if i < 0 {
						break
					}
					line := lineBuf[:i+1] // include newline
					if isImportantLogLine(string(line)) {
						imp.Write(line)
					}
					lineBuf = lineBuf[i+1:]
				}
				if len(lineBuf) > 1<<20 { // guard a pathological no-newline stream
					lineBuf = lineBuf[:0]
				}
			}
		}
		if err != nil {
			return
		}
	}
}
