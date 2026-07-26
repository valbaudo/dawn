package plan

import (
	"strings"
	"testing"
)

// Two `dawn run` against one --dir corrupt nothing — journal lines are atomic and
// blobs are content-addressed. They both MISS the same key and both pay, which is
// what an overnight cron does when a run overruns its interval.
func TestLockRunIsExclusiveAndReleasable(t *testing.T) {
	dir := t.TempDir()
	release, err := LockRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockRun(dir); err == nil {
		t.Fatal("a second run against the same state dir must be refused")
	} else if !strings.Contains(err.Error(), "pay twice") {
		t.Fatalf("the error should say why it matters, got: %v", err)
	}

	// A different state dir is a different run and is never blocked.
	other, err := LockRun(t.TempDir())
	if err != nil {
		t.Fatalf("an unrelated state dir must not be blocked: %v", err)
	}
	other()

	release()
	again, err := LockRun(dir)
	if err != nil {
		t.Fatalf("the lock must be released with the run: %v", err)
	}
	again()
}
