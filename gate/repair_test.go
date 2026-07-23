package gate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/valbaudo/aw"
)

// contentJudge approves iff the candidate (delivered as in.Prompt) contains the
// word "good" — a stand-in for a quality bar the repair loop must satisfy.
type contentJudge struct{ name string }

func (c contentJudge) Name() string { return c.name }
func (c contentJudge) Invoke(_ context.Context, in aw.Invocation) (aw.Result, error) {
	return aw.Result{Output: map[string]any{
		"approved": strings.Contains(in.Prompt, "good"),
		"reason":   "must contain the word good",
	}}, nil
}

func threeContentJudges() []aw.Backend {
	return []aw.Backend{contentJudge{"a"}, contentJudge{"b"}, contentJudge{"c"}}
}

// scripted returns a Generate that yields seq in order (repeating the last),
// recording the feedback it was handed on each call.
func scripted(seen *[]string, seq ...string) Generate {
	i := 0
	return func(_ context.Context, feedback string) (string, error) {
		*seen = append(*seen, feedback)
		s := seq[i]
		if i < len(seq)-1 {
			i++
		}
		return s, nil
	}
}

func TestGatePassesFirstTry(t *testing.T) {
	var fb []string
	out, err := Gate(context.Background(), scripted(&fb, "a good draft"),
		threeContentJudges(), "sys", Majority(3), 3)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Approved || out.Attempts != 1 {
		t.Fatalf("want approved on attempt 1, got %+v", out)
	}
}

func TestGateRepairsAfterFeedback(t *testing.T) {
	var fb []string
	out, err := Gate(context.Background(), scripted(&fb, "a bad draft", "a good draft"),
		threeContentJudges(), "sys", Majority(3), 3)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Approved {
		t.Fatalf("expected approval after one repair, got %+v", out)
	}
	if out.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", out.Attempts)
	}
	if fb[0] != "" {
		t.Fatalf("attempt 1 feedback should be empty, got %q", fb[0])
	}
	if !strings.Contains(fb[1], "REJECTED") {
		t.Fatalf("attempt 2 should carry the rejection critique, got %q", fb[1])
	}
}

func TestGateExhaustsIsBoundedRejection(t *testing.T) {
	var fb []string
	out, err := Gate(context.Background(), scripted(&fb, "a bad draft"),
		threeContentJudges(), "sys", Majority(3), 2)
	if err != nil {
		t.Fatalf("exhausting attempts is not an error: %v", err)
	}
	if out.Approved {
		t.Fatalf("should not approve a persistently bad draft")
	}
	if out.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (maxAttempts)", out.Attempts)
	}
}

func TestGateGenerateErrorPropagates(t *testing.T) {
	gen := func(context.Context, string) (string, error) { return "", errors.New("gen boom") }
	_, err := Gate(context.Background(), gen, threeContentJudges(), "sys", Majority(3), 3)
	if err == nil {
		t.Fatal("a generator failure must propagate, not be swallowed")
	}
}

// A judge that could not deliver a verdict is a mechanical failure: it must
// propagate as an error, never be counted as a rejection that burns an attempt.
func TestGateJudgeErrorIsMechanicalNotRejection(t *testing.T) {
	var fb []string
	judges := []aw.Backend{
		contentJudge{"a"},
		fakeJudge{name: "flaky", err: errors.New("timeout")},
		contentJudge{"c"},
	}
	out, err := Gate(context.Background(), scripted(&fb, "a good draft"),
		judges, "sys", Majority(3), 3)
	if err == nil {
		t.Fatal("a judge error must surface as a mechanical failure")
	}
	if out.Approved {
		t.Fatal("a mechanically failed evaluation must not report approval")
	}
	if len(fb) != 1 {
		t.Fatalf("mechanical failure must not consume repair attempts, generated %d times", len(fb))
	}
}
