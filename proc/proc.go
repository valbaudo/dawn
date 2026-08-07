// Package proc runs child processes so a cancelled context actually reaps them.
//
// The default [exec.CommandContext] kills only the direct child. An agent CLI
// spawns its own tool subprocesses, and those grandchildren inherit the stdout
// pipe: kill the CLI alone and the pipe stays open, so Wait blocks forever and a
// timeout silently becomes a hang. That is the failure this package exists to
// prevent — a supervisor whose timeouts do not fire is worse than no timeout,
// because it looks like slow work.
//
// On Unix, every child therefore gets its own process group and cancellation
// signals the whole group. On non-Unix platforms cancellation kills only the
// direct child. On every platform a WaitDelay bounds how long Wait will block
// on inherited pipes.
package proc

import (
	"context"
	"os/exec"
	"time"
)

// WaitDelay bounds the wait for inherited pipes to close after the process is
// gone. On Unix it backs up process-group cancellation when a grandchild escapes
// the group (a double-fork or new session); elsewhere it bounds pipe waits after
// the direct child is killed.
const WaitDelay = 5 * time.Second

// Command builds a child process that is killed when ctx is cancelled. On Unix
// cancellation kills its whole process group; elsewhere it kills the direct
// child. Callers set Dir, Env, Stdout and Stderr as usual.
//
// Stdin is nil by default: `claude -p` and friends drain an inherited stdin and
// hang waiting for input that a non-interactive caller never sends.
func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	setpgid(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = WaitDelay
	cmd.Stdin = nil
	return cmd
}
