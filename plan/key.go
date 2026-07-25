package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

// keyVersion is the key SCHEMA version. Bumping it misses every entry in every
// journal at once, which is the entire migration story: old work is not rewritten
// or deleted, it simply stops being found.
const keyVersion = 1

// stepKey is exactly what a step's identity is a hash of. It exists as a struct
// so the contents are reviewable in one place — the hard part of a cache key is
// not the hashing, it is agreeing on what goes in.
//
// json.Marshal sorts map keys, so canonicalization is free.
type stepKey struct {
	V       int               `json:"v"`
	ID      string            `json:"id"`
	Backend string            `json:"backend"`
	Model   string            `json:"model"`
	Prompt  string            `json:"prompt"`
	Output  map[string]Type   `json:"output"`           // RESOLVED: the defaulted map
	Inputs  map[string]string `json:"inputs,omitempty"` // name -> resolved ref URI or scalar
	Gate    *gateKey          `json:"gate,omitempty"`
}

type gateKey struct {
	Judges   []string `json:"judges"` // sorted: reordering a panel must be free
	Criteria string   `json:"criteria"`
	Quorum   int      `json:"quorum"` // RESOLVED, so writing the default is free
}

// Key is a step's identity: a claim about the QUESTION ASKED, never about the
// answer. Two runs that would ask a byte-identical question of a byte-identical
// agent about byte-identical inputs share a key.
//
// resolved maps each input name to the upstream's resolved ref URI, or to the
// resolved scalar VALUE. Using the value (not the upstream's key) buys early
// cutoff: if a re-run produces identical bytes, descendants are correctly skipped.
//
// Deliberately NOT in the key: wall clock, run id, hostname, cwd, PID, attempt
// number, prior verdicts, tokens, cost, latency, the plan file as a whole, any
// other step, and the agent CLI's version. Anything that varies per run would
// make every run a miss; anything global would make every edit a global miss.
// Gate `attempts` is out too — a result accepted under 3 attempts is equally
// accepted under 5, which is the definition of policy rather than identity.
func (s Step) Key(a Agent, resolved map[string]string) (string, error) {
	k := stepKey{
		V: keyVersion, ID: s.ID,
		Backend: a.Backend, Model: a.Model,
		Prompt: s.Prompt,
		Output: s.Fields(),
		Inputs: resolved,
	}
	if g := s.Gate; g != nil {
		judges := slices.Clone(g.Judges)
		slices.Sort(judges)
		// Threshold(), not a local copy: the hashed quorum and the enforced quorum
		// are the same call, so they cannot drift.
		k.Gate = &gateKey{Judges: judges, Criteria: g.Criteria, Quorum: g.Threshold()}
	}
	b, err := json.Marshal(k)
	if err != nil {
		return "", fmt.Errorf("plan: key: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
