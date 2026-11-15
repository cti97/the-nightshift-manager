package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestStubReplaysAScriptedScript(t *testing.T) {
	ctx := context.Background()
	script := []Response{
		{Content: "reading the brief", Usage: Usage{In: 12, Out: 4}, Stop: "end_turn"},
		{
			ToolCalls: []Call{{
				ID:        "call-1",
				Name:      "read_file",
				Arguments: json.RawMessage(`{"path":"/tmp/brief.md"}`),
			}},
			Usage: Usage{In: 21, Out: 9},
			Stop:  "tool_use",
		},
	}
	s := NewStub(script)

	// Expectations are literals, not a clone of the script: a clone would share
	// whatever the script shares, so a stub that copied only the slice would
	// still compare equal after both sides were mutated together.
	type wantCall struct{ id, name, args string }
	type wantResp struct {
		content string
		stop    string
		usage   Usage
		calls   []wantCall
	}
	want := []wantResp{
		{content: "reading the brief", stop: "end_turn", usage: Usage{In: 12, Out: 4}},
		{
			stop:  "tool_use",
			usage: Usage{In: 21, Out: 9},
			calls: []wantCall{{"call-1", "read_file", `{"path":"/tmp/brief.md"}`}},
		},
	}

	// The stub copies its script, including the tool-call argument bytes: a
	// caller that keeps a handle on the slice it passed in cannot change the
	// schedule. The byte write is in place on purpose — replacing the field
	// would only change the caller's own struct, while the copy is what
	// protects the shared backing array.
	script[0].Content = "mutated"
	script[1].ToolCalls[0].Arguments[0] = 'X'

	for i := range want {
		got, err := s.Complete(ctx, Request{Model: "stub-model"})
		if err != nil {
			t.Fatalf("Complete %d: %v", i, err)
		}
		if got.Content != want[i].content {
			t.Fatalf("Complete %d content = %q, want %q", i, got.Content, want[i].content)
		}
		if got.Stop != want[i].stop {
			t.Fatalf("Complete %d stop = %q, want %q", i, got.Stop, want[i].stop)
		}
		if got.Usage != want[i].usage {
			t.Fatalf("Complete %d usage = %+v, want %+v", i, got.Usage, want[i].usage)
		}
		if len(got.ToolCalls) != len(want[i].calls) {
			t.Fatalf("Complete %d returned %d tool calls, want %d", i, len(got.ToolCalls), len(want[i].calls))
		}
		for j, w := range want[i].calls {
			g := got.ToolCalls[j]
			if g.ID != w.id || g.Name != w.name || !bytes.Equal(g.Arguments, []byte(w.args)) {
				t.Fatalf("Complete %d call %d = %+v, want id %q name %q arguments %s", i, j, g, w.id, w.name, w.args)
			}
		}
	}

	// Exhaustion is an error, never an empty response: a stub that silently
	// returned nothing would let a test pass on work it never modelled. It keeps
	// erroring rather than resetting, so a loop cannot wrap around.
	for i := 0; i < 2; i++ {
		if _, err := s.Complete(ctx, Request{Model: "stub-model"}); !errors.Is(err, ErrScriptExhausted) {
			t.Fatalf("call %d past the end of the script: err = %v, want ErrScriptExhausted", i+1, err)
		}
	}
	if _, err := NewStub(nil).Complete(ctx, Request{Model: "stub-model"}); !errors.Is(err, ErrScriptExhausted) {
		t.Fatalf("empty script: err = %v, want ErrScriptExhausted", err)
	}
}

func TestStubRefusesWhenLocalRequiredButReachIsWAN(t *testing.T) {
	ctx := context.Background()
	script := []Response{{Content: "scripted reply", Usage: Usage{In: 8, Out: 2}, Stop: "end_turn"}}

	// "wan" is the case spec §10 names. The rest are the same hole spelled
	// differently: only an exact "host" or "lan" proves locality, so an unknown
	// or differently-cased reach refuses too. Guessing here would be egress.
	for _, reach := range []string{"wan", "", "WAN", " wan", "wan ", "remote", "host,lan"} {
		s := NewStub(script)
		resp, err := s.Complete(ctx, Request{Model: "m", RequireLocal: true, Reach: reach})
		if err == nil {
			t.Fatalf("reach %q: a local-only request was sent (content %q)", reach, resp.Content)
		}
		if !errors.Is(err, ErrEgressRefused) {
			t.Fatalf("reach %q: err = %v, want ErrEgressRefused", reach, err)
		}
		if resp.Content != "" || len(resp.ToolCalls) != 0 || resp.Usage != (Usage{}) {
			t.Fatalf("reach %q: the refusal returned a response: %+v", reach, resp)
		}
	}

	// The endpoint's configured reach is checked too: Config.Reach is the
	// declaration the adapter was built with, so a caller that omits Reach
	// cannot bypass the check by saying nothing.
	configuredWAN := NewStub(script)
	configuredWAN.Reach = "wan"
	if _, err := configuredWAN.Complete(ctx, Request{Model: "m", RequireLocal: true}); !errors.Is(err, ErrEgressRefused) {
		t.Fatalf("configured wan endpoint: err = %v, want ErrEgressRefused", err)
	}

	// A refusal happens before the script is consumed, exactly as a real adapter
	// refuses before writing the request: the stub's next local call still gets
	// the first scripted response.
	refused := NewStub(script)
	if _, err := refused.Complete(ctx, Request{Model: "m", RequireLocal: true, Reach: "wan"}); err == nil {
		t.Fatal("reach wan: sent a local-only request")
	}
	got, err := refused.Complete(ctx, Request{Model: "m", RequireLocal: true, Reach: "host"})
	if err != nil {
		t.Fatalf("local call after a refused wan call: %v (the refusal must not consume a scripted response)", err)
	}
	if got.Content != "scripted reply" {
		t.Fatalf("local call after a refused wan call returned %q, want the first scripted response", got.Content)
	}

	// host and lan are the two classes that satisfy no_egress.
	for _, reach := range []string{"host", "lan"} {
		s := NewStub(script)
		got, err := s.Complete(ctx, Request{Model: "m", RequireLocal: true, Reach: reach})
		if err != nil {
			t.Fatalf("reach %q: %v", reach, err)
		}
		if got.Content != "scripted reply" {
			t.Fatalf("reach %q returned %q, want the scripted response", reach, got.Content)
		}
	}

	// Without RequireLocal the reach is not a policy question: a wan endpoint is
	// exactly what a normal run uses.
	s := NewStub(script)
	if _, err := s.Complete(ctx, Request{Model: "m", Reach: "wan"}); err != nil {
		t.Fatalf("reach wan without RequireLocal: %v", err)
	}
}

func TestEstimateTokensIsMonotonic(t *testing.T) {
	s := NewStub(nil)
	base := Request{
		Model:    "stub-model",
		System:   "you are a careful worker",
		Messages: []Message{{Role: "user", Content: "read the brief and plan the work"}},
		Tools: []ToolDef{{
			Name:        "read_file",
			Description: "read a file",
			Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
	}
	baseTokens := s.EstimateTokens(base)
	if baseTokens <= 0 {
		t.Fatalf("EstimateTokens(base) = %d, want a positive count for a non-empty request", baseTokens)
	}
	if got := s.EstimateTokens(Request{}); got != 0 {
		t.Fatalf("EstimateTokens(empty) = %d, want 0", got)
	}

	clone := func(r Request) Request {
		out := r
		out.Messages = append([]Message(nil), r.Messages...)
		out.Tools = append([]ToolDef(nil), r.Tools...)
		return out
	}
	var longer []struct {
		name string
		req  Request
	}
	add := func(name string, mutate func(*Request)) {
		r := clone(base)
		mutate(&r)
		longer = append(longer, struct {
			name string
			req  Request
		}{name, r})
	}

	// Every input field the estimator reads, grown substantially. These are
	// asserted strictly greater, not merely non-decreasing: an estimator that
	// returned a constant would pass a >= check and be useless to a budget.
	add("a longer system prompt", func(r *Request) { r.System += strings.Repeat("; be precise", 8) })
	add("an appended message", func(r *Request) {
		r.Messages = append(r.Messages, Message{Role: "assistant", Content: "here is the plan"})
	})
	add("a longer message body", func(r *Request) {
		r.Messages[0].Content += strings.Repeat(" and then verify it", 8)
	})
	add("a longer message name", func(r *Request) { r.Messages[0].Name = "brief_reader" })
	add("a longer tool description", func(r *Request) {
		r.Tools[0].Description += strings.Repeat("; it must not escape the sandbox", 4)
	})
	add("a longer tool schema", func(r *Request) {
		r.Tools[0].Schema = json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"absolute path of the file to read"},"limit":{"type":"integer"}},"required":["path"]}`)
	})
	add("an appended tool", func(r *Request) {
		r.Tools = append(r.Tools, ToolDef{Name: "finish", Description: "finish the run", Schema: json.RawMessage(`{"type":"object"}`)})
	})
	// A turn with no content still costs the delimiters that frame it.
	add("an empty appended message", func(r *Request) { r.Messages = append(r.Messages, Message{}) })

	for _, tc := range longer {
		if got := s.EstimateTokens(tc.req); got <= baseTokens {
			t.Errorf("%s: EstimateTokens = %d, want > %d (a longer request must not estimate fewer tokens)",
				tc.name, got, baseTokens)
		}
	}

	// The property itself, one byte at a time: rounding may leave the estimate
	// unchanged, but growing the input must never lower it.
	prev := s.EstimateTokens(Request{})
	for i := 1; i <= 64; i++ {
		req := Request{Model: "stub-model", Messages: []Message{{Role: "user", Content: strings.Repeat("a", i)}}}
		got := s.EstimateTokens(req)
		if got < prev {
			t.Fatalf("content of %d bytes estimates %d tokens, fewer than %d for %d bytes", i, got, prev, i-1)
		}
		prev = got
	}
}
