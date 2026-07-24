//go:build unix

package proc

import (
	"os/exec"
	"syscall"
)

// setpgid puts the child in a new process group whose id is the child's pid, so
// the group can be signalled as a unit.
func setpgid(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup signals the child's whole process group. A negative pid means "the
// group with this id", which is why setpgid above makes the group id equal the
// child's pid. Falls back to killing the direct child if the group is already
// gone (a race with normal exit).
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
