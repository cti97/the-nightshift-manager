package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// The tests in this file are the token-estimation contract documented in
// estimate.go. They are written against the package default because every
// adapter returns it; a family that overrides EstimateTokens with a real
// tokenizer must keep the same invariants, so these are properties of the
// Provider interface rather than of one implementation.

// TestEstimateTokensIsMonotoneUnderAppend grows a conversation the way the
// executor does: turn by turn, appending to the history that was already sent.
// Monotonicity under a rebuild is not enough — a growing conversation is the
// case the budget actually meets, and an estimate that fell when a turn was
// appended would under-reserve exactly when the context is largest.
func TestEstimateTokensIsMonotoneUnderAppend(t *testing.T) {
	base := Request{System: "you are a careful worker"}
	turns := []Message{
		{Role: "user", Content: "read the brief and plan the work"},
		{Role: "assistant", ToolCalls: []Call{{
			ID:        "call-1",
			Name:      "read_file",
			Arguments: json.RawMessage(`{"path":"/tmp/brief.md"}`),
		}}},
		{Role: "tool", Name: "read_file", CallID: "call-1", Content: "the brief is short: verify before claiming done"},
		{Role: "assistant", Content: "the plan is: read, verify, report"},
		{Role: "user", Content: "now do it"},
	}

	prev := estimateTokens(base)
	for i := range turns {
		// Append in place to the request that was already sent; do not rebuild
		// the history from a fresh slate.
		cur := base
		cur.Messages = append(cur.Messages, turns[:i+1]...)
		got := estimateTokens(cur)
		if got <= prev {
			t.Fatalf("after appending turn %d (%q): estimate %d, want > %d; appending a turn must strictly grow the estimate",
				i, turns[i].Role, got, prev)
		}
		prev = got
	}

	// The estimator is pure: rebuilding the same history from scratch must not
	// land on a different number, or two code paths in M5 would reserve
	// differently for the same bytes.
	rebuilt := Request{System: base.System}
	rebuilt.Messages = append(rebuilt.Messages, turns...)
	if got, want := estimateTokens(rebuilt), prev; got != want {
		t.Errorf("rebuilt history estimates %d, appended history %d; the estimate must depend only on the request", got, want)
	}
}

// TestEstimateTokensCountsEveryInputField changes exactly one input field at a
// time and requires the estimate to grow. The table is the guard against a
// field that goes on the wire but is silently dropped from the estimate: a
// tool schema and a tool-result body are prompt tokens, and both are the easy
// ones to forget. The number of turns and tools is held fixed, so framing
// cannot account for the growth.
func TestEstimateTokensCountsEveryInputField(t *testing.T) {
	base := Request{
		System: "you are a careful worker",
		Messages: []Message{
			{Role: "user", Name: "operator", Content: "read the brief"},
			{Role: "assistant", ToolCalls: []Call{{
				ID:        "call-1",
				Name:      "read_file",
				Arguments: json.RawMessage(`{"path":"/tmp/brief.md"}`),
			}}},
		},
		Tools: []ToolDef{{
			Name:        "read_file",
			Description: "read a file",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
	}
	baseTokens := estimateTokens(base)
	if baseTokens <= 0 {
		t.Fatalf("EstimateTokens(base) = %d, want a positive count for a non-empty request", baseTokens)
	}

	cases := []struct {
		name   string
		mutate func(*Request)
	}{
		{"a longer system prompt", func(r *Request) {
			r.System += strings.Repeat("; be precise", 8)
		}},
		{"a longer message role", func(r *Request) {
			r.Messages[0].Role = "user" + strings.Repeat("_role", 4)
		}},
		{"a longer message name", func(r *Request) {
			r.Messages[0].Name = "brief_operator"
		}},
		{"a longer message content", func(r *Request) {
			r.Messages[0].Content += strings.Repeat(" and then verify it", 8)
		}},
		{"a longer message call id", func(r *Request) {
			// Empty in the base; a tool result's id goes on the wire.
			r.Messages[0].CallID = "call-1"
		}},
		{"a longer call id", func(r *Request) {
			r.Messages[1].ToolCalls[0].ID = "call-" + strings.Repeat("1", 32)
		}},
		{"a longer call name", func(r *Request) {
			r.Messages[1].ToolCalls[0].Name = "read_file_" + strings.Repeat("x", 16)
		}},
		{"longer call arguments", func(r *Request) {
			r.Messages[1].ToolCalls[0].Arguments = json.RawMessage(`{"path":"/tmp/` + strings.Repeat("a", 64) + `"}`)
		}},
		{"a longer tool name", func(r *Request) {
			r.Tools[0].Name = "read_file_" + strings.Repeat("x", 16)
		}},
		{"a longer tool description", func(r *Request) {
			r.Tools[0].Description += strings.Repeat("; it must not escape the sandbox", 4)
		}},
		{"a larger tool schema", func(r *Request) {
			r.Tools[0].Schema = json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer"},"offset":{"type":"integer"}},"required":["path"]}`)
		}},
		{"an appended message", func(r *Request) {
			r.Messages = append(r.Messages, Message{Role: "tool", Name: "read_file", CallID: "call-1", Content: "ok"})
		}},
		{"an appended tool", func(r *Request) {
			r.Tools = append(r.Tools, ToolDef{Name: "finish", Description: "finish the run", Schema: json.RawMessage(`{"type":"object"}`)})
		}},
	}
	for _, tc := range cases {
		r := cloneRequest(base)
		tc.mutate(&r)
		if got := estimateTokens(r); got <= baseTokens {
			t.Errorf("%s: EstimateTokens = %d, want > %d; the field is sent on the wire and must be counted",
				tc.name, got, baseTokens)
		}
	}
}

// TestEstimateTokensChargesForToolSchemasAndResults keeps the two largest
// forgettable fields honest with a byte floor. A 400-byte schema or tool-result
// body must move the estimate by at least 50 tokens (one per eight bytes); the
// production ratio is one per four, so the floor is loose but an estimator that
// dropped either field scores zero and fails. These are prompt tokens: the
// model reads the schema before it answers and the result body on the next turn.
func TestEstimateTokensChargesForToolSchemasAndResults(t *testing.T) {
	bigSchema := json.RawMessage(`{"type":"object","description":"` + strings.Repeat("x", 400) + `"}`)
	withSchema := Request{Tools: []ToolDef{{Name: "t", Schema: bigSchema}}}
	withoutSchema := Request{Tools: []ToolDef{{Name: "t"}}}
	if delta := estimateTokens(withSchema) - estimateTokens(withoutSchema); delta < len(bigSchema)/8 {
		t.Errorf("a %d-byte tool schema moved the estimate by %d, want >= %d; tool schemas are prompt tokens",
			len(bigSchema), delta, len(bigSchema)/8)
	}

	result := strings.Repeat("the tool result body ", 40)
	withResult := Request{Messages: []Message{{Role: "tool", CallID: "call-1", Content: result}}}
	withoutResult := Request{Messages: []Message{{Role: "tool", CallID: "call-1"}}}
	if delta := estimateTokens(withResult) - estimateTokens(withoutResult); delta < len(result)/8 {
		t.Errorf("a %d-byte tool result moved the estimate by %d, want >= %d; the result body is sent back as prompt input",
			len(result), delta, len(result)/8)
	}
}

// TestEstimateTokensFloorAndZero pins the boundary: the zero Request is 0,
// every request that carries input is at least 1, and no input is negative.
// Model and MaxTokens select and cap rather than carry prompt text, so a
// request with only those has no input tokens to reserve — they are tested as 0
// so the exclusion is a decision, not an accident.
func TestEstimateTokensFloorAndZero(t *testing.T) {
	if got := estimateTokens(Request{}); got != 0 {
		t.Errorf("EstimateTokens(zero request) = %d, want 0", got)
	}

	inputBearing := []struct {
		name string
		req  Request
	}{
		{"a one-byte system prompt", Request{System: "x"}},
		{"a whitespace-only system prompt", Request{System: " "}},
		{"a one-byte message", Request{Messages: []Message{{Content: "x"}}}},
		{"a message with no content", Request{Messages: []Message{{}}}},
		{"a tool with no fields", Request{Tools: []ToolDef{{}}}},
		{"a message carrying an empty call", Request{Messages: []Message{{ToolCalls: []Call{{}}}}}},
		{"a zero-length schema", Request{Tools: []ToolDef{{Schema: json.RawMessage{}}}}},
	}
	for _, tc := range inputBearing {
		if got := estimateTokens(tc.req); got < 1 {
			t.Errorf("%s: EstimateTokens = %d, want >= 1 for a request that carries input", tc.name, got)
		}
	}

	// Not input: a model selector and an output cap. A request carrying only
	// these is not sendable, and reserving prompt tokens for them would charge
	// for bytes that never appear in the prompt.
	for _, tc := range []struct {
		name string
		req  Request
	}{
		{"a model name only", Request{Model: "gpt-4o"}},
		{"a reply cap only", Request{MaxTokens: 256}},
		{"a reach/egress declaration only", Request{Reach: "host", RequireLocal: true}},
	} {
		if got := estimateTokens(tc.req); got != 0 {
			t.Errorf("%s: EstimateTokens = %d, want 0; Model, MaxTokens and reach are not prompt input", tc.name, got)
		}
	}
}

// TestEstimateTokensWithinFactorOfTwoOfAHandCount is the brief's acceptance
// sample. The hand count is independent of the implementation: it counts prose
// in words at 4/3 tokens per word (the same rate the conformance suite's oracle
// uses) plus five tokens of chat framing per turn. The samples are prose-only
// on purpose — there is no hand-countable tokenizer for dense JSON, so schema
// and tool-result coverage is asserted by growth and a byte floor above rather
// than smuggled into a word count.
func TestEstimateTokensWithinFactorOfTwoOfAHandCount(t *testing.T) {
	paragraph := "The night shift is quiet, and the work that remains is the work nobody watches. " +
		"Reading a brief, planning the next step, and writing down what was learned is the whole job. " +
		"A careful worker keeps the plan short, checks each step, and reports what changed. " +
		"Nothing is guessed, and nothing is left implicit."

	turns := func(n int, content string) []Message {
		out := make([]Message, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, Message{Role: "user", Content: content})
		}
		return out
	}

	samples := []struct {
		name string
		msgs []Message
	}{
		{"one short turn", turns(1, "read the brief and plan the work")},
		{"one paragraph", turns(1, paragraph)},
		{"four paragraphs in one turn", turns(1, strings.Repeat(paragraph+"\n\n", 4))},
		{"four turns of one paragraph", turns(4, paragraph)},
		{"eight turns of one paragraph", turns(8, paragraph)},
	}

	for _, tc := range samples {
		req := Request{Messages: tc.msgs}
		got := estimateTokens(req)

		words := 0
		for _, m := range tc.msgs {
			words += len(strings.Fields(m.Content))
		}
		// 4/3 tokens per word of prose, plus the framing a chat template adds
		// per turn (role token and separators), counted by hand at five.
		hand := float64(words)*4/3 + float64(len(tc.msgs))*5
		ratio := float64(got) / hand
		t.Logf("%s: estimate %d, hand count %.1f, ratio %.2f", tc.name, got, hand, ratio)

		if ratio < 0.5 || ratio > 2 {
			t.Errorf("%s: estimate %d is %.2fx the hand count of %.1f, want within a factor of two",
				tc.name, got, ratio, hand)
		}
	}
}

// cloneRequest deep-copies the slices a mutation could grow, so each table row
// starts from the same base rather than from the previous row's changes.
func cloneRequest(r Request) Request {
	out := r
	out.Messages = make([]Message, len(r.Messages))
	copy(out.Messages, r.Messages)
	for i := range out.Messages {
		out.Messages[i].ToolCalls = append([]Call(nil), r.Messages[i].ToolCalls...)
	}
	out.Tools = append([]ToolDef(nil), r.Tools...)
	return out
}
