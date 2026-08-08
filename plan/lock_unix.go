//go:build (unix && !solaris && !aix) || illumos

// The set where syscall.Flock EXISTS, which is not any one build tag.
//
// Go's `unix` tag includes solaris and aix, and neither has Flock — so
// `//go:build unix` alone meant the plan package did not compile there at all.
// But excluding `solaris` alone then over-corrected: Go sets the `solaris` tag
// for GOOS=illumos too, and illumos DOES have Flock (syscall_illumos.go exists
// precisely because it does). That silently handed illumos the no-op lock, and a
// cross-BUILD check could never catch it — illumos compiled fine, just without
// the guarantee. Probed: flock on illumos, linux, darwin, freebsd, netbsd,
// openbsd, dragonfly; absent on solaris, aix, windows, plan9, js, wasip1.

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
