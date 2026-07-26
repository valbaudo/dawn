//go:build !unix

package plan

import "os"

// flock is POSIX. Elsewhere concurrent runs are unguarded: they still corrupt
// nothing, they just both pay. Matching proc's pgid split rather than dropping
// the platform.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) {}
