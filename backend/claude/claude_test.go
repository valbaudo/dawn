package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valbaudo/aw"
)

// extractJSON is the adapter's parse boundary. It must never invent a value for
// a reply that holds no JSON: a placeholder flows downstream as data, and a
// caller reading a missing field scores it as a real answer.
func TestExtractJSONParses(t *testing.T) {
	t.Run("bare object", func(t *testing.T) {
		m, err := extractJSON(`{"approved": true, "reason": "fine"}`)
		if err != nil {
			t.Fatal(err)
		}
		if m["approved"] != true || m["reason"] != "fine" {
			t.Fatalf("got %v", m)
		}
	})

	t.Run("code fence and surrounding prose", func(t *testing.T) {
		m, err := extractJSON("Sure! Here you go:\n```json\n{\"approved\": false, \"reason\": \"nope\"}\n```")
		if err != nil {
			t.Fatal(err)
		}
		if m["approved"] != false || m["reason"] != "nope" {
			t.Fatalf("got %v", m)
		}
	})
}

func TestExtractJSONRejectsNonJSON(t *testing.T) {
	for name, in := range map[string]string{
		"refusal":     "I can't help with that request.",
		"rate limit":  "Error: rate limit exceeded, try again later",
		"empty":       "",
		"broken json": `{"approved": tru`,
	} {
		t.Run(name, func(t *testing.T) {
			m, err := extractJSON(in)
			if err == nil {
				t.Fatalf("a reply with no JSON object must error, got %v", m)
			}
			if m != nil {
				t.Fatal("must not return a placeholder value alongside an error")
			}
		})
	}
}

func TestName(t *testing.T) {
	if got := (Backend{Model: "opus"}).Name(); got != "claude:opus" {
		t.Fatalf("Name() = %q", got)
	}
	if got := (Backend{}).Name(); got != "claude" {
		t.Fatalf("Name() = %q", got)
	}
	if got := (Workspace{Model: "sonnet"}).Name(); !strings.HasPrefix(got, "claude-ws") {
		t.Fatalf("Name() = %q", got)
	}
}

// fakeCLI writes an executable stand-in for `claude` and returns its path.
func fakeCLI(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// Unattended plus no deadline means a hung tool call burns the night looking like
// slow work. Every invocation is bounded, and because proc.Command runs the child
// in its own process group, a grandchild holding stdout cannot keep it alive past
// the deadline either.
func TestInvokeTimesOut(t *testing.T) {
	bin := fakeCLI(t, "sleep 30 & sleep 30")
	b := Backend{Model: "haiku", Bin: bin, Timeout: 300 * time.Millisecond}

	start := time.Now()
	_, err := b.Invoke(context.Background(), aw.Invocation{Prompt: "hi"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a hung CLI must fail, not hang")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Invoke took %v: the deadline did not reap the process group", elapsed)
	}
}

// A CLI that answers promptly is unaffected by the bound.
func TestInvokeUnderTheDeadlineSucceeds(t *testing.T) {
	bin := fakeCLI(t, `echo '{"type":"result","is_error":false,"result":"pong","usage":{"input_tokens":3,"output_tokens":1}}'`)
	b := Backend{Model: "haiku", Bin: bin}

	res, err := b.Invoke(context.Background(), aw.Invocation{Prompt: "ping"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := res.Output["text"].(string); got != "pong" {
		t.Fatalf("Output[text] = %q", got)
	}
	if res.Tokens.Input != 3 {
		t.Fatalf("token usage did not survive: %+v", res.Tokens)
	}
}

// The default applies when a caller sets nothing.
func TestTimeoutDefaults(t *testing.T) {
	if got := timeoutOr(0); got != DefaultTimeout {
		t.Fatalf("zero must resolve to the default, got %v", got)
	}
	if got := timeoutOr(2 * time.Second); got != 2*time.Second {
		t.Fatalf("an explicit timeout must win, got %v", got)
	}
}
