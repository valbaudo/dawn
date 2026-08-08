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
	"syscall"
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

// setpgid puts the child in a new process group whose id is the child's pid, so
// the group can be signalled as a unit.
//
// No build tag, for the reason in platform.go: the non-Unix version of this file
// killed only the direct child, which is precisely the bug this package exists to
// prevent — an agent CLI's tool subprocesses keep the inherited pipe open and the
// timeout becomes a hang that looks like slow work. A guarantee that quietly
// degrades on a platform nobody tested is worse than one that refuses to build.
func setpgid(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup signals the child's whole process group. A negative pid means "the
// group with this id", which is why setpgid makes the group id equal the child's
// pid. Falls back to the direct child if the group is already gone (a race with
// normal exit).
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
