package urnettools

import (
	"context"
	"os/exec"
	"time"
)

// execCommandFn is the subprocess seam under execWithTimeout. Tests replace it
// to record which commands a discovery pass runs and to make one of them hang.
var execCommandFn = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// execWithTimeout runs a command with a deadline, preventing wedged
// systemctl/getent/docker subprocesses from hanging the entire tool.
func execWithTimeout(timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return execCommandFn(ctx, name, args...)
}
