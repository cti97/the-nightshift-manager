package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

// The conformance suite injects its four failure cases through the request's
// model name, because Provider has no transport of its own: the suite never owns
// the endpoint, so it cannot make a server answer 401. A conformance fixture —
// the httptest server inside an adapter's test, or the scripted stub — answers a
// request for one of these names with the matching failure:
//
//	ConformanceModelAuth            401 Unauthorized
//	ConformanceModelRateLimited     429 Too Many Requests
//	ConformanceModelContextOverflow 400 with the family's context-too-long body
//	ConformanceModelServerError     500 Internal Server Error
//
// The names are reserved: no real model is called any of these, and an adapter
// passes them through untouched.
const (
	ConformanceModelAuth            = "conformance-auth"
	ConformanceModelRateLimited     = "conformance-rate-limited"
	ConformanceModelContextOverflow = "conformance-context-overflow"
	ConformanceModelServerError     = "conformance-server-error"
)

const (
	// conformanceModel is the ordinary model the suite asks for. The fake
	// endpoint's default branch answers it; only the four names above select
	// failures.
	conformanceModel = "conformance-model"
	// conformanceToolName is the one tool the suite offers. A fake endpoint
	// returns a call to it; the stub's script must too.
	conformanceToolName = "read_file"
)

// Conformance runs the checks every provider must pass against h. A provider
// that fails here does not exist (design §10).
//
// newProvider must construct a fresh provider directly — not through
// Registry.New, which would test the registry instead of the adapter — and point
// it at the conformance fake endpoint. The suite calls newProvider once at the
// start of each check, so no check can pass on state another one left behind.
//
// Provider has no transport the suite can reach, so the fake endpoint is the
// caller's half of the contract. For an HTTP adapter it is an httptest server
// speaking that adapter's wire shape; for the scripted stub it is a script. It
// must:
//
//   - answer an ordinary request with non-empty content and positive Usage;
//   - answer a request that offers tools with at least one Call whose Name is
//     one of the offered tools (the suite offers read_file) and whose Arguments
//     are valid JSON, and answer the follow-up request that carries the tool
//     result (Message.Role "tool") with a final completion that calls no tool;
//   - report the same Usage for the same request twice;
//   - answer the four ConformanceModel* names with the matching failure.
//
// The provider must also be built with an unset Config.Reach. Check 6 asserts
// that local-only work is refused when the endpoint's reach was never
// configured, even when the request claims "host": a reach that cannot be
// verified is not local, and an adapter that forgets to carry its endpoint's
// reach must fail closed (M2 Task 2). The converse — a provider built with a
// local reach accepts local-only work — cannot be expressed through
// func() Provider, which cannot rebuild the provider with a different Config; it
// is covered by the stub's own tests and, once endpoints land, by their wiring.
//
// The stub is the reference implementation: it answers the ConformanceModel*
// names itself (conformanceFailure in stub.go), so the suite can be passed with
// no endpoint present.
func Conformance(t *testing.T, newProvider func() Provider) {
	t.Helper()
	if newProvider == nil {
		t.Fatal("provider: Conformance called with a nil newProvider")
	}
	for _, check := range conformanceChecks() {
		t.Run(check.name, func(t *testing.T) {
			t.Helper()
			p := newProvider()
			if p == nil {
				t.Fatalf("%s: newProvider returned a nil provider", check.name)
			}
			if err := check.run(context.Background(), p); err != nil {
				t.Fatalf("%s: %v", check.name, err)
			}
		})
	}
}

// conformanceCheck is one check. It returns an error instead of failing a
// *testing.T because a test cannot assert that another *testing.T failed: the
// suite's own test runs these against a deliberately broken provider and counts
// the verdicts. Conformance is the only place a *testing.T is touched, and it
// turns every error into a t.Fatalf in its own subtest, so a failing check stops
// nothing but itself.
type conformanceCheck struct {
	name string
	run  func(ctx context.Context, p Provider) error
}

func conformanceChecks() []conformanceCheck {
	return []conformanceCheck{
		{name: "completion_returns_content_and_usage", run: checkCompletion},
		{name: "tool_call_round_trip", run: checkToolRoundTrip},
		{name: "usage_is_stable", run: checkUsageStable},
		{name: "estimate_is_within_forty_percent", run: checkTokenEstimate},
		{name: "errors_map_to_the_taxonomy", run: checkErrorMapping},
		{name: "refuses_local_only_without_a_local_reach", run: checkReachRefusal},
	}
}

// checkCompletion is check 1. A tool call alongside the content is allowed: the
// OpenAI-shaped APIs may return both, and check 2 is the one that requires the
// call. What matters here is that a plain request produces something the
// executor can report and usage the budget can settle against.
func checkCompletion(ctx context.Context, p Provider) error {
	resp, err := p.Complete(ctx, plainRequest(conformanceModel))
	if err != nil {
		return fmt.Errorf("Complete: %w", err)
	}
	if strings.TrimSpace(resp.Content) == "" {
		return fmt.Errorf("Complete returned no content (response %+v)", resp)
	}
	if resp.Usage.In <= 0 || resp.Usage.Out <= 0 {
		return fmt.Errorf("Complete reported Usage %+v; want positive In and Out so the call can be settled", resp.Usage)
	}
	return nil
}

// checkToolRoundTrip is check 2: the provider emits a call, accepts the tool
// result as a later message in the same conversation, and finishes.
//
// Message has no field for the assistant's tool_calls or the call ID, so this
// exercises the Provider-level round trip, not the wire shape: whether a real
// endpoint needs the assistant turn and tool_call_id is the adapter's own
// acceptance test. The gap is recorded in the M2 Task 2 report.
func checkToolRoundTrip(ctx context.Context, p Provider) error {
	req := plainRequest(conformanceModel)
	req.Tools = conformanceTools()

	first, err := p.Complete(ctx, req)
	if err != nil {
		return fmt.Errorf("first Complete (tool offered): %w", err)
	}
	if len(first.ToolCalls) == 0 {
		return fmt.Errorf("Complete returned no tool call for a request offering %q (response %+v)", conformanceToolName, first)
	}
	call := first.ToolCalls[0]
	if strings.TrimSpace(call.ID) == "" {
		return fmt.Errorf("tool call %+v has no ID; its result cannot be correlated with it", call)
	}
	if call.Name != conformanceToolName {
		return fmt.Errorf("tool call names %q, but the only tool offered was %q", call.Name, conformanceToolName)
	}
	if !json.Valid(call.Arguments) {
		return fmt.Errorf("tool call arguments %s are not valid JSON; the tool layer hands them to json.Unmarshal", call.Arguments)
	}

	req.Messages = append(req.Messages, Message{
		Role:    "tool",
		Name:    call.Name,
		Content: `{"content":"# The brief"}`,
	})
	finished, err := p.Complete(ctx, req)
	if err != nil {
		return fmt.Errorf("second Complete (tool result): %w", err)
	}
	if len(finished.ToolCalls) != 0 {
		return fmt.Errorf("after the tool result Complete returned %d more tool calls; the round trip did not finish", len(finished.ToolCalls))
	}
	if strings.TrimSpace(finished.Content) == "" {
		return fmt.Errorf("after the tool result Complete returned no final content")
	}
	return nil
}

// checkUsageStable is check 3: the same request must settle the same way twice.
// A provider that counts a call, includes latency or jitters a random value
// makes the budget unreconcilable after the fact.
func checkUsageStable(ctx context.Context, p Provider) error {
	req := plainRequest(conformanceModel)
	first, err := p.Complete(ctx, req)
	if err != nil {
		return fmt.Errorf("first Complete: %w", err)
	}
	second, err := p.Complete(ctx, req)
	if err != nil {
		return fmt.Errorf("second Complete: %w", err)
	}
	if first.Usage != second.Usage {
		return fmt.Errorf("two identical requests reported Usage %+v and %+v; usage must be stable", first.Usage, second.Usage)
	}
	return nil
}

// checkTokenEstimate is check 4: EstimateTokens must be within ±40% of actual.
//
// "Actual" cannot be a real tokenizer — the suite runs no model, and a
// provider's reported Usage is whatever its endpoint (or, for the stub, its
// script) says, which is test data rather than a measurement of the request. A
// stub's scripted usage is deliberately unrelated to its input, so comparing the
// estimate to it would test the script author's arithmetic. The suite therefore
// uses an oracle it owns: the prose it placed in the request, counted in words.
// English prose averages about 4/3 tokens per word for every family this
// project talks to, and ±40% is wide enough for a real tokenizer while far too
// narrow for a constant or mis-scaled estimate. The growth assertion catches a
// constant estimate that happens to sit inside the band.
func checkTokenEstimate(_ context.Context, p Provider) error {
	short, long := proseRequest(1), proseRequest(4)
	shortGot, longGot := p.EstimateTokens(short), p.EstimateTokens(long)

	var problems []error
	for _, tc := range []struct {
		name string
		got  int
		req  Request
	}{
		{"one paragraph", shortGot, short},
		{"four paragraphs", longGot, long},
	} {
		want := wordTokenCount(tc.req)
		if tc.got <= 0 {
			problems = append(problems, fmt.Errorf("%s: EstimateTokens = %d; want a positive count", tc.name, tc.got))
			continue
		}
		if off := math.Abs(float64(tc.got)-want) / want; off > 0.40 {
			problems = append(problems, fmt.Errorf("%s: EstimateTokens = %d, want %.0f ±40%% (off by %.0f%%), at about 4/3 tokens per word of prose",
				tc.name, tc.got, want, off*100))
		}
	}
	if longGot <= shortGot {
		problems = append(problems, fmt.Errorf("four paragraphs estimated %d tokens, not more than %d for one; the estimate does not grow with its input", longGot, shortGot))
	}
	return errors.Join(problems...)
}

// checkErrorMapping is check 5. Every failure is collected before returning, so
// an adapter author sees all four classes at once instead of fixing one per run.
func checkErrorMapping(ctx context.Context, p Provider) error {
	cases := []struct {
		model string
		want  error
	}{
		{ConformanceModelAuth, ErrAuth},
		{ConformanceModelRateLimited, ErrRateLimited},
		{ConformanceModelContextOverflow, ErrContextOverflow},
		{ConformanceModelServerError, ErrServer},
	}
	var problems []error
	for _, tc := range cases {
		resp, err := p.Complete(ctx, plainRequest(tc.model))
		if err == nil {
			problems = append(problems, fmt.Errorf("model %q returned a response (%+v); want an error satisfying errors.Is(err, %v)", tc.model, resp, tc.want))
			continue
		}
		if !errors.Is(err, tc.want) {
			problems = append(problems, fmt.Errorf("model %q: err = %v; want an error satisfying errors.Is(err, %v)", tc.model, err, tc.want))
		}
	}
	return errors.Join(problems...)
}

// checkReachRefusal is check 6. The conformance provider is built without a
// configured reach, so none of these declarations is verifiably local: "wan"
// contradicts, and "host" or an absent declaration cannot be verified because
// the endpoint itself declared nothing. Fail-closed means all three refuse —
// trusting the request's claim is the hole where an adapter that forgot to carry
// Config.Reach would send no_egress work to whatever it was pointed at.
func checkReachRefusal(ctx context.Context, p Provider) error {
	var problems []error
	for _, reach := range []string{"wan", "host", ""} {
		req := plainRequest(conformanceModel)
		req.RequireLocal = true
		req.Reach = reach
		resp, err := p.Complete(ctx, req)
		switch {
		case err == nil:
			problems = append(problems, fmt.Errorf("reach %q: a local-only request was sent (response %+v); the conformance provider is built without an endpoint reach", reach, resp))
		case !errors.Is(err, ErrEgressRefused):
			problems = append(problems, fmt.Errorf("reach %q: err = %v; want ErrEgressRefused", reach, err))
		}
	}
	return errors.Join(problems...)
}

func plainRequest(model string) Request {
	return Request{
		Model: model,
		// Anthropic requires max_tokens and the other shapes accept it; the
		// suite supplies it so an adapter does not have to invent a reply cap.
		MaxTokens: 256,
		Messages:  []Message{{Role: "user", Content: "Read the brief and say what it asks for."}},
	}
}

func conformanceTools() []ToolDef {
	return []ToolDef{{
		Name:        conformanceToolName,
		Description: "Read a file from the run directory and return its text.",
		Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
	}}
}

// conformanceProse is ordinary English on purpose: the word-count oracle depends
// on the text having normal word lengths, not on a repeated filler string that
// would let a byte-count estimator look calibrated when it is not.
const conformanceProse = "The night shift is quiet, and the work that remains is the work nobody watches: " +
	"reading a brief, planning the next step, and writing down what was learned. " +
	"A careful worker keeps the plan short, checks each step, and reports what changed. " +
	"Nothing is guessed."

func proseRequest(paragraphs int) Request {
	return Request{
		Model:     conformanceModel,
		MaxTokens: 256,
		Messages:  []Message{{Role: "user", Content: strings.Repeat(conformanceProse+"\n\n", paragraphs)}},
	}
}

// wordTokenCount is the suite's independent token oracle for check 4. It counts
// only the fields that carry prose, so the provider's framing overhead (a few
// tokens per turn) has to fit inside the ±40% band rather than being measured as
// prose.
func wordTokenCount(req Request) float64 {
	words := 0
	count := func(s string) { words += len(strings.Fields(s)) }
	count(req.System)
	for _, m := range req.Messages {
		count(m.Content)
	}
	for _, t := range req.Tools {
		count(t.Description)
	}
	return float64(words) * 4 / 3
}
