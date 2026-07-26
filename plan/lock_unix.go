//go:build unix

package plan

import (
	"os"
	"syscall"
)

// LOCK_NB, never a blocking wait: an overrunning cron should be told it is late,
// not queued behind the run it is duplicating.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockFile(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
