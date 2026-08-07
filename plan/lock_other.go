//go:build !unix

package plan

import "os"

// The run lock is a Unix-only guarantee. On other platforms this no-op keeps
// the binary available, but concurrent runs against one state directory are
// unguarded and can execute the same work and pay twice.
func lockFile(*os.File) error { return nil }

func unlockFile(*os.File) {}
