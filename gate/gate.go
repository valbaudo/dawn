// Package gate is dawn's differentiator, kept deliberately as a LIBRARY, not an
// engine primitive: an independent acceptance check over any set of
// [dawn.Backend]s. A single judge is [Judge]; N judges with a k-of-N quorum is
// [Jury]. Both are a fan-out of independent [dawn.Backend.Invoke] calls plus a
// count — no runtime privileges, no shared state, no journal ownership. The
// spike proved this needs zero engine support, and this file is that proof.
//
// Independence is structural: each judge is its own Invoke with a fresh context;
// judges never see each other's votes. Diversity (different models) is the value,
// which is exactly why a jury shares no cache prefix.
package gate

import (
	"context"
	"fmt"
	"sync"

	"github.com/valbaudo/dawn"
)

// newVerdictSchema returns the typed contract one judge must return. Every call
// allocates the top-level map, nested maps, and required slice because backends
// may normalize or otherwise mutate an invocation schema during concurrent use.
func newVerdictSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"approved", "reason"},
		"properties": map[string]any{
			"approved": map[string]any{"type": "boolean"},
			"reason":   map[string]any{"type": "string"},
		},
	}
}

// Verdict is one judge's vote. A judge whose Invoke failed carries Err and never
// counts as an approval.
type Verdict struct {
	Judge    string
	Approved bool
	Reason   string
	Err      error
}

// Majority is the default quorum for n judges: n/2 + 1. It fails closed on an
// even split (e.g. 2 of 2, 3 of 4), so a tie rejects rather than passes.
func Majority(n int) int { return n/2 + 1 }

// Judge runs one evaluator over candidate and returns its typed verdict. It is
// Jury with a single judge; use it when one evaluator is enough.
func Judge(ctx context.Context, judge dawn.Backend, system, candidate string) Verdict {
	res, err := judge.Invoke(ctx, dawn.Invocation{
		System: system,
		Prompt: candidate,
		Schema: newVerdictSchema(),
	})
	v := Verdict{Judge: judge.Name()}
	if err != nil {
		v.Err = err
		return v
	}
	// A judge that did not return a usable verdict has not voted. Silently
	// reading a missing or non-bool "approved" as false would turn a parse
	// failure into a quality rejection: it would burn a repair attempt and
	// terminate the gate with an empty critique, indistinguishable from a real
	// bounded rejection. That is exactly the crash-becomes-verdict confusion
	// this package exists to prevent, so it is an error, not a no.
	approved, ok := res.Output["approved"].(bool)
	if !ok {
		v.Err = fmt.Errorf("judge %s: no boolean \"approved\" in verdict (got %v)", v.Judge, res.Output)
		return v
	}
	v.Approved = approved
	v.Reason, _ = res.Output["reason"].(string)
	return v
}

// Jury runs each judge independently and concurrently over the same candidate
// and returns whether approvals reached quorum, along with every vote. Votes are
// returned in judge order; an errored judge is a non-approval, never a panic.
func Jury(ctx context.Context, judges []dawn.Backend, system, candidate string, quorum int) (approved bool, votes []Verdict) {
	votes = make([]Verdict, len(judges))
	var wg sync.WaitGroup
	for i, j := range judges {
		wg.Add(1)
		go func(i int, j dawn.Backend) {
			defer wg.Done()
			votes[i] = Judge(ctx, j, system, candidate)
		}(i, j)
	}
	wg.Wait()
	yes := 0
	for _, v := range votes {
		if v.Err == nil && v.Approved {
			yes++
		}
	}
	return yes >= quorum, votes
}
