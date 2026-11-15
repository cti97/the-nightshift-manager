package provider

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

// conformanceScript is the scripted transcript the stub replays for the suite.
// The first response carries content *and* a tool call because two checks read
// it: check 1 wants content plus usage from a plain request, and check 2 wants a
// call from the request that offers a tool. The two responses carry the same
// usage because check 3 compares them across two identical runs.
func conformanceScript() []Response {
	return []Response{
		{
			Content: "The brief asks for the provider seam and a way to prove it.",
			ToolCalls: []Call{{
				ID:        "call-1",
				Name:      conformanceToolName,
				Arguments: json.RawMessage(`{"path":"/tmp/brief.md"}`),
			}},
			Usage: Usage{In: 120, Out: 30},
			Stop:  "tool_use",
		},
		{
			Content: "The file was read and the brief is planned.",
			Usage:   Usage{In: 120, Out: 30},
			Stop:    "end_turn",
		},
	}
}

// TestStubPassesConformance is the reference-implementation proof: the suite can
// be passed, and the stub is what it is passed against. The stub is built
// directly (not through Registry.New) and with no endpoint reach, which is the
// fixture contract check 6 asserts.
func TestStubPassesConformance(t *testing.T) {
	Conformance(t, func() Provider {
		return NewStub(conformanceScript())
	})
}

// TestConformanceRunsEveryCheck guards the count the brief fixes at six. A check
// dropped from the list would otherwise leave a silent hole: the suite would
// still pass.
func TestConformanceRunsEveryCheck(t *testing.T) {
	checks := conformanceChecks()
	if len(checks) != 6 {
		t.Fatalf("conformanceChecks() returned %d checks, want 6", len(checks))
	}
	seen := make(map[string]bool, len(checks))
	for _, c := range checks {
		if c.name == "" {
			t.Errorf("a check has no name")
		}
		if c.run == nil {
			t.Errorf("check %q has no run function", c.name)
		}
		if seen[c.name] {
			t.Errorf("check name %q appears more than once", c.name)
		}
		seen[c.name] = true
	}
}

// brokenProvider is the suite's negative control. It is deliberately broken in
// exactly three ways, each silent enough that a weaker suite would pass it:
//
//   - a tool call never survives Complete, so the tool round trip cannot finish;
//   - Usage drifts by one token per call, so two identical requests disagree;
//   - every taxonomy failure is reported as ErrAuth, so three of the four
//     classes are wrong.
//
// Everything else works (content, usage, the estimator, the fail-closed reach
// check), so the three failures are attributable to the three defects rather
// than to a provider that returns garbage.
type brokenProvider struct {
	stub  *Stub
	calls int
}

func newBrokenProvider() Provider {
	return &brokenProvider{stub: NewStub(conformanceScript())}
}

func (b *brokenProvider) Name() string { return "broken" }

func (b *brokenProvider) EstimateTokens(req Request) int { return b.stub.EstimateTokens(req) }

func (b *brokenProvider) Probe(ctx context.Context) (Health, error) { return b.stub.Probe(ctx) }

func (b *brokenProvider) Complete(ctx context.Context, req Request) (Response, error) {
	resp, err := b.stub.Complete(ctx, req)
	if err != nil {
		// Defect 3: only auth survives; the other three taxonomy classes are
		// flattened into it. Egress refusals are not taxonomy errors and must
		// pass through untouched, or check 6 would be measuring this defect.
		if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrContextOverflow) || errors.Is(err, ErrServer) {
			return Response{}, ErrAuth
		}
		return resp, err
	}
	b.calls++
	// Defect 1.
	resp.ToolCalls = nil
	// Defect 2.
	resp.Usage.In += b.calls
	return resp, nil
}

// TestConformanceFailsADeliberatelyBrokenProvider is the proof that the suite
// has teeth. It runs the checks directly rather than through Conformance
// because a test cannot assert that another *testing.T failed; the checks return
// errors for exactly this reason, and Conformance turns each error into a
// t.Fatalf in its own subtest.
func TestConformanceFailsADeliberatelyBrokenProvider(t *testing.T) {
	wantFailed := []string{
		"tool_call_round_trip",
		"usage_is_stable",
		"errors_map_to_the_taxonomy",
	}

	var got []string
	for _, check := range conformanceChecks() {
		if err := check.run(context.Background(), newBrokenProvider()); err != nil {
			got = append(got, check.name)
		}
	}

	if len(got) < 3 {
		t.Fatalf("the deliberately broken provider failed %d checks (%v), want at least 3", len(got), got)
	}
	for _, name := range wantFailed {
		if !slices.Contains(got, name) {
			t.Errorf("broken provider passed %q; the suite is too weak to catch that defect", name)
		}
	}
	// The other direction matters too: a failure outside the three defects means
	// a check is reporting something other than what it claims to test.
	for _, name := range got {
		if !slices.Contains(wantFailed, name) {
			t.Errorf("broken provider failed %q, which its defects do not touch; the failure is not attributable", name)
		}
	}
}

// TestStubRefusesConformanceFailureModelWithoutSpendingItsScript pins the part
// of the fixture contract the suite depends on: the stub answers a reserved
// failure model with the taxonomy error, before it consumes a scripted
// response, so check 5 cannot exhaust the transcript or depend on its order.
func TestStubRefusesConformanceFailureModelWithoutSpendingItsScript(t *testing.T) {
	ctx := context.Background()
	s := NewStub(conformanceScript())

	want := map[string]error{
		ConformanceModelAuth:            ErrAuth,
		ConformanceModelRateLimited:     ErrRateLimited,
		ConformanceModelContextOverflow: ErrContextOverflow,
		ConformanceModelServerError:     ErrServer,
	}
	for model, sentinel := range want {
		if _, err := s.Complete(ctx, Request{Model: model}); !errors.Is(err, sentinel) {
			t.Fatalf("model %q: err = %v, want %v", model, err, sentinel)
		}
	}

	// Four failure calls spent nothing: the first scripted response is still
	// there for a real request.
	resp, err := s.Complete(ctx, Request{Model: "conformance-model"})
	if err != nil {
		t.Fatalf("call after four failure models: %v", err)
	}
	if resp.Content != conformanceScript()[0].Content {
		t.Fatalf("call after four failure models returned %q, want the first scripted response", resp.Content)
	}
}
