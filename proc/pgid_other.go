//go:build !unix

package proc

import "os/exec"

// Process groups are a POSIX concept; elsewhere fall back to killing the direct
// child. WaitDelay still bounds the wait on inherited pipes.
func setpgid(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
