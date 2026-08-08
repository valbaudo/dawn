//go:build !darwin && !linux

package dawn

// dawn supports macOS and Linux. Building anywhere else stops here, on purpose,
// with the identifier below as the message.
//
// The alternative was what shipped before: build everywhere, and hand the
// unsupported platforms a no-op. On Windows that meant `lockFile` returned nil,
// so "one run per state directory" was not enforced and two runs both paid; and
// `killGroup` killed only the direct child, so a timed-out agent left its tool
// subprocesses holding the inherited pipe — the exact hang the proc package
// exists to prevent. Both guarantees are documented. Neither held. Nothing said
// so, because a binary that builds looks like a binary that works.
//
// Adding a platform is therefore a DECISION, not a fallback: implement the two
// primitives there (on Windows, LockFileEx and a Job Object, which need
// golang.org/x/sys/windows and would be dawn's second dependency), prove them,
// and delete a term from the constraint above. Until someone does that, WSL is
// Linux and works today.
//
// The refusal lives in the root package because every other package imports it,
// so one file covers the whole module.
var _ = dawn_supports_macOS_and_Linux_only__see_platform_go
