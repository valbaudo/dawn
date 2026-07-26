package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/valbaudo/dawn/plan"
)

// The exit code is the entire interface an unattended caller has to the run.
//
// The load-bearing case is the last one: an interrupt cancels every in-flight
// Invoke, so the run surfaces as whatever the cancelled call returned. Classify
// on the error alone and Ctrl-C reports "the machine broke" — or worse, a
// cancelled gate reports "the panel refused".
func TestExitCode(t *testing.T) {
	rejected := fmt.Errorf("step %q: %w", "draft", &plan.RejectedError{Attempts: 3, Objections: "too vague"})

	for _, tc := range []struct {
		name        string
		interrupted bool
		err         error
		want        int
	}{
		{"the panel refused", false, rejected, exitRefused},
		{"the author typed it wrong", false, usagef("missing PLAN"), exitUsage},
		{"the machine broke", false, errors.New("connection reset by peer"), exitMechanical},
		{"interrupted", true, errors.New("context canceled"), exitInterrupted},
		{"interrupt outranks a mechanical error", true, errors.New("connection reset by peer"), exitInterrupted},
		{"interrupt outranks a rejection", true, rejected, exitInterrupted},
	} {
		if got := exitCode(tc.interrupted, tc.err); got != tc.want {
			t.Errorf("%s: exitCode(%v, %v) = %d, want %d", tc.name, tc.interrupted, tc.err, got, tc.want)
		}
	}
}
