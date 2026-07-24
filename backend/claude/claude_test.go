package claude

import (
	"strings"
	"testing"
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
