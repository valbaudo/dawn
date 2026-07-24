package proc

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// The regression this package exists for: a child that spawned a grandchild
// holding the stdout pipe. Killing only the direct child leaves the pipe open,
// so Run blocks until WaitDelay (or forever, without one) and the timeout never
// really fires. With a process-group kill both die at once and Run returns
// immediately.
//
// The assertion is timing, and it discriminates: WaitDelay is 5s, so a run that
// relied on it alone would take ~5s. Returning in well under that proves the
// group kill did the work.
func TestCancelReapsGrandchildren(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// `sleep 30 &` inherits stdout and outlives its parent shell.
	cmd := Command(ctx, "sh", "-c", "sleep 30 & sleep 30")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the cancelled context to fail the run")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("Run took %v: the process group was not reaped (a grandchild held the pipe "+
			"and only WaitDelay=%v released it)", elapsed, WaitDelay)
	}
}

// A process that exits on its own must still deliver output normally.
func TestCommandRunsNormally(t *testing.T) {
	cmd := Command(context.Background(), "sh", "-c", "echo hello")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "hello\n" {
		t.Fatalf("stdout = %q, want %q", got, "hello\n")
	}
}

// A nonzero exit is reported as an error, not swallowed.
func TestCommandPropagatesExitCode(t *testing.T) {
	cmd := Command(context.Background(), "sh", "-c", "exit 3")
	if err := cmd.Run(); err == nil {
		t.Fatal("a nonzero exit must be an error")
	}
}
