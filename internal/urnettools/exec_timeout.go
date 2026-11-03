package urnettools

import (
	"context"
	"os/exec"
	"sync"
	"time"
)

// execCommandFn is the subprocess seam under execWithTimeout. Tests replace it
// to record which commands a discovery pass runs and to make one of them hang.
var (
	execMu        sync.RWMutex
	execCommandFn = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).Output()
	}
)

// execWithTimeout runs a command with a deadline, preventing wedged
// systemctl/getent/docker subprocesses from hanging the entire tool.
func execWithTimeout(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	execMu.RLock()
	fn := execCommandFn
	execMu.RUnlock()
	return fn(ctx, name, args...)
}
