//go:build (!unix || solaris || aix) && !illumos

package plan

import "os"

// The run lock needs syscall.Flock: present on darwin, linux, illumos and the
// BSDs, absent on solaris, aix, windows, plan9 and js/wasm. On those this
// no-op keeps the binary available, and concurrent runs against one state
// directory are unguarded: both miss the same key, both execute it, both pay.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) {}
