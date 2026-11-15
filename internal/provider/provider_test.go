package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestCheckToolMessagesRefusesAnEmptyCallID pins the provider-layer half of the
// tool contract independently of the stub's endpoint-side attribution check. An
// adapter calls this helper at the top of Complete; check 2 exercises the same
// refusal through the seam, but a strict fixture can produce it too, so this test
// is what keeps the helper itself honest.
func TestCheckToolMessagesRefusesAnEmptyCallID(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "read the brief"},
		{Role: "assistant", ToolCalls: []Call{{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{}`)}}},
		{Role: "tool", Name: "read_file", Content: `{"content":"# The brief"}`},
	}
	err := checkToolMessages(Request{Messages: msgs})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("checkToolMessages with a tool turn that has no CallID = %v; want ErrBadRequest", err)
	}
	if !strings.Contains(err.Error(), "message 2") {
		t.Errorf("checkToolMessages = %v; want the offending turn named", err)
	}

	msgs[2].CallID = "call-1"
	if err := checkToolMessages(Request{Messages: msgs}); err != nil {
		t.Fatalf("checkToolMessages with CallID %q: %v", msgs[2].CallID, err)
	}

	// The helper is about tool turns: a stray CallID on another role is a caller
	// mistake, but the result is not a tool result and there is nothing to
	// attribute, so it passes through.
	if err := checkToolMessages(Request{Messages: []Message{{Role: "user", CallID: "stray"}}}); err != nil {
		t.Fatalf("checkToolMessages on a user turn with a stray CallID: %v", err)
	}
}
