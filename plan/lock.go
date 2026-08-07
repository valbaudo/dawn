package plan

import (
	"fmt"
	"os"
	"path/filepath"
)

// LockRun takes an exclusive, non-blocking lock on a state directory for the
// duration of a run on Unix, and returns its release. On non-Unix platforms it
// opens the lock file but performs no interprocess locking.
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
