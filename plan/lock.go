package plan

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockRun takes an exclusive, non-blocking lock on a state directory for the
// duration of a run, and returns its release.
//
// Two `dawn run` processes against one --dir corrupt nothing: journal lines are
// atomic O_APPEND writes and blobs are content-addressed, so the log stays
// readable and every ref stays valid. They simply both MISS the same key, both
// execute it, and both pay. That is the failure this prevents — silent double
// token spend, which is exactly what an overnight cron does when a run overruns
// its own interval and the next one starts on top of it. Nothing in the output
// would tell you; the journal just has two lines where you expected one.
//
// flock, not a pid file: the kernel drops it when the process dies, so a run
// killed with SIGKILL leaves nothing to clean up and there is no stale-lock
// heuristic to get wrong. It is also why the lock file's CONTENTS are never read.
//
// `dawn show` never takes it. Reading committed state is always safe, and a
// preview that blocked on a running job would be its own 3am annoyance.
func LockRun(dir string) (release func(), err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("lock: mkdir %s: %w", dir, err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock: open: %w", err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another dawn run holds %s: concurrent runs against one state dir "+
			"re-execute the same steps and pay twice", dir)
	}
	return func() {
		unlockFile(f)
		_ = f.Close()
	}, nil
}

// LOCK_NB, never a blocking wait: an overrunning cron should be told it is late,
// not queued behind the run it is duplicating.
//
// No build tag. dawn supports macOS and Linux, both of which have flock, and an
// unsupported platform fails to build rather than receiving a no-op — see
// platform.go. The previous arrangement shipped a silent downgrade: on Windows
// this function returned nil, so the guarantee above simply did not hold and
// nothing said so.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockFile(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
