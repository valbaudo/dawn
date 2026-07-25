package plan

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Entry is one journal line. Exactly two fields are load-bearing — Key and Ref —
// and everything else is provenance that is never hashed and never consulted for
// a decision.
//
// A line with no Ref records something that happened but produced nothing
// reusable: a panel that refused. Recording it is forensics; refusing to serve it
// is the point. The model is nondeterministic, so the honest response to "this
// was rejected last night" is to run it again.
type Entry struct {
	Key      string `json:"key"`
	Ref      string `json:"ref,omitempty"`
	Step     string `json:"step"`
	Agent    string `json:"agent,omitempty"`
	Rejected string `json:"rejected,omitempty"`
	At       string `json:"at"`
}

// Journal is an append-only log of what a step's key resolved to. It is the whole
// of aw's durable run state: there is no separate state file, and no resume mode.
// `aw run` looks up each key and skips what it finds, so re-running IS resuming —
// one code path, exercised on every run rather than only after a crash.
type Journal struct{ path string }

// OpenJournal opens (creating if needed) the journal under dir.
func OpenJournal(dir string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("journal: mkdir %s: %w", dir, err)
	}
	return &Journal{path: filepath.Join(dir, "journal.jsonl")}, nil
}

// Lookup returns the ref most recently committed for key. Only an entry carrying
// a ref can serve a hit, so a rejection never satisfies a later run.
//
// ponytail: scans the file per call. A journal is one short line per committed
// step; index it if a plan ever gets big enough for this to show up.
func (j *Journal) Lookup(key string) (string, bool) {
	f, err := os.Open(j.path)
	if err != nil {
		return "", false // no journal yet: everything is a miss
	}
	defer f.Close()
	var ref string
	var found bool
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // a torn or future-format line must not break a lookup
		}
		if e.Key == key && e.Ref != "" {
			ref, found = e.Ref, true // keep scanning: newest wins
		}
	}
	return ref, found
}

// Append records one entry durably. The caller must have committed the entry's
// blob FIRST: a crash between the two leaves an orphan blob, which is harmless
// garbage, whereas the other order would leave a journal pointer to bytes that do
// not exist.
//
// ponytail: O_APPEND of a short line is atomic on POSIX, which covers concurrent
// runs against one journal without a lock. Rejection reasons are elided to keep
// lines short enough for that to hold.
func (j *Journal) Append(e Entry) error {
	if e.At == "" {
		e.At = time.Now().UTC().Format(time.RFC3339)
	}
	if len(e.Rejected) > 500 {
		e.Rejected = e.Rejected[:497] + "..."
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("journal: marshal: %w", err)
	}
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("journal: open: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("journal: write: %w", err)
	}
	return f.Sync()
}

// Entries reads the whole journal in order, for inspection.
func (j *Journal) Entries() ([]Entry, error) {
	f, err := os.Open(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}
