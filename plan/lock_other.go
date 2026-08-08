//go:build !unix || solaris || aix

package plan

import "os"

// The run lock needs syscall.Flock, which Go provides on darwin, linux, illumos
// and the BSDs — but NOT on solaris, aix, windows, plan9 or js. On those this
// no-op keeps the binary available, and concurrent runs against one state
// directory are unguarded: both miss the same key, both execute it, both pay.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) {}
