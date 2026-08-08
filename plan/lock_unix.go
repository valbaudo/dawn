//go:build unix && !solaris && !aix

// Go's `unix` build tag includes solaris and aix, and NEITHER has syscall.Flock
// — so `//go:build unix` alone did not mean "flock is available here", it meant
// the plan package did not compile there at all. The two are named explicitly
// rather than listing the platforms that do work, so a future GOOS that Go adds
// to `unix` WITH flock is picked up automatically and one without it fails loudly
// at build time instead of silently losing the lock.

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
