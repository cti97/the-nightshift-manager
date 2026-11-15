package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestCheckToolMessagesRequiresAPrecedingCall pins the provider-layer rule
// independently of the stub: an adapter inherits it by calling the helper, and
// check 2 only sees the observable (ErrBadRequest). Every refusal case here is one
// an adapter that merely rejects empty strings would get wrong.
func TestCheckToolMessagesRequiresAPrecedingCall(t *testing.T) {
	call := Call{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{}`)}
	assistant := Message{Role: "assistant", ToolCalls: []Call{call}}
	result := func(callID string) Message {
		return Message{Role: "tool", Name: "read_file", CallID: callID, Content: `{"content":"# The brief"}`}
	}

	cases := []struct {
		name string
		msgs []Message
		want string
	}{
		{"no call id", []Message{assistant, result("")}, "no call id"},
		{"an unknown call id", []Message{assistant, result("call-2")}, "no preceding assistant turn carries"},
		{"no assistant turn at all", []Message{result("call-1")}, "no preceding assistant turn carries"},
		{"the call is answered before it is made", []Message{result("call-1"), assistant}, "no preceding assistant turn carries"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkToolMessages(Request{Messages: tc.msgs})
			if !errors.Is(err, ErrBadRequest) {
				t.Fatalf("checkToolMessages = %v; want ErrBadRequest", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("checkToolMessages = %v; want the %q refusal", err, tc.want)
			}
			if !strings.Contains(err.Error(), "message ") {
				t.Errorf("checkToolMessages = %v; want the offending turn named", err)
			}
		})
	}

	// The positive case, and a stray CallID outside a tool turn (a caller
	// mistake this refusal is not about).
	if err := checkToolMessages(Request{Messages: []Message{assistant, result("call-1")}}); err != nil {
		t.Fatalf("checkToolMessages with an attributable result: %v", err)
	}
	if err := checkToolMessages(Request{Messages: []Message{{Role: "user", CallID: "stray"}}}); err != nil {
		t.Fatalf("checkToolMessages on a user turn with a stray CallID: %v", err)
	}
}
