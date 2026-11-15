package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// conformanceScript is the scripted transcript the stub replays for the suite.
// The first response carries content *and* a tool call because two checks read
// it: check 1 wants content plus usage from a plain request, and check 2 wants a
// call from the request that offers a tool. The two responses carry the same
// usage because check 3 compares them across two identical runs.
//
// Both report positive usage on purpose: the Usage doc says a text response that
// reports none is broken, and check 1 refuses it. The script is the fake
// endpoint's answer, so a zero here would be a broken fixture, not a provider
// that is allowed to omit usage.
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

// correlationDroppingProvider wraps a correct provider and damages the request
// the way a translation layer with a bug would: the correlation data is dropped
// on the floor before the endpoint sees it. check 2 has to notice, or it only
// proves that messages are accepted.
type correlationDroppingProvider struct {
	Provider
	drop func(*Message)
}

func (p correlationDroppingProvider) Complete(ctx context.Context, req Request) (Response, error) {
	// A real adapter would not mutate the caller's slice; the copy keeps this
	// fake from corrupting the suite's own request.
	msgs := make([]Message, len(req.Messages))
	copy(msgs, req.Messages)
	for i := range msgs {
		p.drop(&msgs[i])
	}
	req.Messages = msgs
	return p.Provider.Complete(ctx, req)
}

// TestConformanceCatchesADroppedCorrelation is the self-review question made
// into a test: does check 2 fail for a provider that drops the correlation id?
// Both halves of the pairing are dropped in turn, and the failure has to be the
// provider refusing the unattributable result with ErrBadRequest — not an
// exhausted script or some unrelated complaint.
func TestConformanceCatchesADroppedCorrelation(t *testing.T) {
	cases := []struct {
		name string
		drop func(*Message)
	}{
		{"the result's CallID", func(m *Message) {
			if m.Role == "tool" {
				m.CallID = ""
			}
		}},
		{"the assistant's ToolCalls", func(m *Message) {
			if m.Role == "assistant" {
				m.ToolCalls = nil
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := correlationDroppingProvider{Provider: NewStub(conformanceScript()), drop: tc.drop}
			err := checkToolRoundTrip(context.Background(), p)
			if err == nil {
				t.Fatalf("check 2 passed after dropping %s; the tool round trip does not require the correlation", tc.name)
			}
			if !errors.Is(err, ErrBadRequest) {
				t.Fatalf("check 2 failed with %v; want the provider to refuse the unattributable result with ErrBadRequest, so the failure is attributable to the drop", err)
			}
		})
	}
}

// TestCheck1RequiresPositiveUsage pins the Usage decision: a text response that
// reports no usage is broken, not merely unknown, because the budget settles
// against it. The Usage doc says so and check 1 enforces it; without this test
// the doc could drift back to permitting zero and only the doc would notice.
func TestCheck1RequiresPositiveUsage(t *testing.T) {
	for _, usage := range []Usage{{}, {In: 5}, {Out: 5}} {
		p := NewStub([]Response{{Content: "The brief asks for a provider seam.", Usage: usage, Stop: "end_turn"}})
		if err := checkCompletion(context.Background(), p); err == nil {
			t.Errorf("check 1 accepted Usage %+v on a text response; a non-positive count cannot be settled", usage)
		}
	}
	p := NewStub([]Response{{Content: "The brief asks for a provider seam.", Usage: Usage{In: 5, Out: 1}, Stop: "end_turn"}})
	if err := checkCompletion(context.Background(), p); err != nil {
		t.Errorf("check 1 rejected a positive Usage: %v", err)
	}
}

// blockingProvider ignores its context: Complete waits for the test to release
// it, so only the runner's own deadline can end the check. This is the "hung
// adapter" the per-check timeout exists for.
type blockingProvider struct {
	release chan struct{}
}

func (p *blockingProvider) Name() string               { return "blocking" }
func (p *blockingProvider) EstimateTokens(Request) int { return 0 }
func (p *blockingProvider) Probe(context.Context) (Health, error) {
	return HealthOK, nil
}
func (p *blockingProvider) Complete(context.Context, Request) (Response, error) {
	<-p.release
	return Response{Content: "late", Usage: Usage{In: 1, Out: 1}}, nil
}

// deadlineProvider respects its context and reports the deadline, which is what
// a well-behaved adapter's HTTP client does.
type deadlineProvider struct{}

func (p *deadlineProvider) Name() string               { return "deadline" }
func (p *deadlineProvider) EstimateTokens(Request) int { return 0 }
func (p *deadlineProvider) Probe(context.Context) (Health, error) {
	return HealthOK, nil
}
func (p *deadlineProvider) Complete(ctx context.Context, _ Request) (Response, error) {
	<-ctx.Done()
	return Response{}, ctx.Err()
}

// ownDeadlineProvider returns its own context.DeadlineExceeded promptly, like a
// provider whose HTTP client timed out internally.
type ownDeadlineProvider struct{}

func (p *ownDeadlineProvider) Name() string               { return "own-deadline" }
func (p *ownDeadlineProvider) EstimateTokens(Request) int { return 0 }
func (p *ownDeadlineProvider) Probe(context.Context) (Health, error) {
	return HealthOK, nil
}
func (p *ownDeadlineProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, fmt.Errorf("adapter: %w", context.DeadlineExceeded)
}

// TestRunCheckDoesNotBlameTheSuiteForAnAdaptersTimeout: an adapter's own timeout
// is its failure, not the suite's. The "did not finish within" message is for a
// check that actually ran out of conformanceCheckTimeout; an adapter that
// returns promptly with its own deadline error must keep its own message.
func TestRunCheckDoesNotBlameTheSuiteForAnAdaptersTimeout(t *testing.T) {
	err := runCheck(conformanceCheck{name: "own deadline", run: func(ctx context.Context, p Provider) error {
		_, err := p.Complete(ctx, Request{Model: conformanceModel})
		return err
	}}, &ownDeadlineProvider{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runCheck = %v; want the adapter's deadline error preserved", err)
	}
	if strings.Contains(err.Error(), "did not finish within") {
		t.Fatalf("runCheck = %v; the adapter's own timeout was reported as a suite timeout", err)
	}
}

// TestConformanceCheckTimesOut proves the timeout is taken, not merely set. The
// second leg is the one that matters: a provider that never looks at its context
// must still fail its own check rather than hang the suite until the go test
// panic, and that is what the runner's goroutine and select are for.
func TestConformanceCheckTimesOut(t *testing.T) {
	old := conformanceCheckTimeout
	conformanceCheckTimeout = 100 * time.Millisecond
	t.Cleanup(func() { conformanceCheckTimeout = old })

	run := func(p Provider) error {
		return runCheck(conformanceCheck{name: "hangs", run: func(ctx context.Context, p Provider) error {
			_, err := p.Complete(ctx, Request{Model: conformanceModel})
			return err
		}}, p)
	}

	// Every leg is watched. A runner with no deadline at all would block the
	// first leg until the go test panic, and because subtests are sequential the
	// second leg's watchdog would never get to report it: a regression test whose
	// failure mode is "hangs the binary" gets deleted as flaky instead of read.
	watched := func(t *testing.T, p Provider) error {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- run(p) }()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("runCheck did not return; a check without a working deadline would hang the suite")
			return nil
		}
	}

	t.Run("a provider that respects the context", func(t *testing.T) {
		err := watched(t, &deadlineProvider{})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("runCheck = %v; want an error wrapping context.DeadlineExceeded", err)
		}
		if !strings.Contains(err.Error(), "did not finish within") {
			t.Errorf("runCheck = %v; the failure does not name the timeout", err)
		}
	})

	t.Run("a provider that ignores the context", func(t *testing.T) {
		p := &blockingProvider{release: make(chan struct{})}
		t.Cleanup(func() { close(p.release) }) // release the abandoned goroutine

		err := watched(t, p)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("runCheck = %v; want an error wrapping context.DeadlineExceeded", err)
		}
		if !strings.Contains(err.Error(), "did not finish within") {
			t.Errorf("runCheck = %v; the failure does not name the timeout", err)
		}
	})
}

// panickingProvider crashes inside Complete, which is what an adapter with a nil
// dereference does.
type panickingProvider struct{}

func (p *panickingProvider) Name() string               { return "panics" }
func (p *panickingProvider) EstimateTokens(Request) int { return 0 }
func (p *panickingProvider) Probe(context.Context) (Health, error) {
	return HealthOK, nil
}
func (p *panickingProvider) Complete(context.Context, Request) (Response, error) {
	panic("the adapter crashed")
}

// TestConformanceCheckRecoversAPanic: a panicking adapter fails its own check,
// with the adapter named, instead of killing the test binary. Without the
// recover in runCheck this test does not fail — it takes the process down.
func TestConformanceCheckRecoversAPanic(t *testing.T) {
	err := runCheck(conformanceCheck{name: "panics", run: func(ctx context.Context, p Provider) error {
		_, err := p.Complete(ctx, Request{Model: conformanceModel})
		return err
	}}, &panickingProvider{})
	if err == nil {
		t.Fatal("runCheck returned nil after the provider panicked")
	}
	if !strings.Contains(err.Error(), "panicked") || !strings.Contains(err.Error(), "panickingProvider") {
		t.Fatalf("runCheck = %v; want the panic reported with the provider named", err)
	}
}

// permissiveProvider answers from its own script and validates nothing, which is
// the shape of an adapter that forwards whatever it is handed. Check 2 must fail
// it on the unattributable result: the last leg exists so a provider cannot pass
// just because its fixture happens to be strict.
type permissiveProvider struct {
	script []Response
	next   int
	// refuse, when non-nil, is consulted for every message before a response is
	// replayed. It is how a subtest makes the provider strict about one thing
	// only, so each refusal leg of check 2 can be given its own teeth.
	refuse func(Message) bool
}

func (p *permissiveProvider) Name() string               { return "permissive" }
func (p *permissiveProvider) EstimateTokens(Request) int { return 1 }
func (p *permissiveProvider) Probe(context.Context) (Health, error) {
	return HealthOK, nil
}
func (p *permissiveProvider) Complete(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if p.refuse != nil {
		for _, m := range req.Messages {
			if p.refuse(m) {
				return Response{}, fmt.Errorf("%w: the permissive provider refuses %+v", ErrBadRequest, m)
			}
		}
	}
	if p.next >= len(p.script) {
		return Response{}, errors.New("script exhausted")
	}
	resp := p.script[p.next]
	p.next++
	return resp, nil
}

// TestCheck2RefusesAnUnattributableResult gives each refusal leg of check 2 its
// own teeth. A provider that refuses only an empty CallID passes the first leg
// and then answers the unknown-id turn, so only the unknown-id assertion can
// fail it — that is the case an adapter with no correlation logic reaches.
func TestCheck2RefusesAnUnattributableResult(t *testing.T) {
	script := append(conformanceScript(), Response{
		Content: "The brief is planned.",
		Usage:   Usage{In: 120, Out: 30},
		Stop:    "end_turn",
	})
	cases := []struct {
		name   string
		refuse func(Message) bool
		want   string
	}{
		{
			name: "a provider that accepts everything",
			want: "no CallID",
		},
		{
			name: "a provider that only rejects an empty CallID",
			refuse: func(m Message) bool {
				return m.Role == "tool" && m.CallID == ""
			},
			want: "unknown call id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkToolRoundTrip(context.Background(), &permissiveProvider{script: script, refuse: tc.refuse})
			if err == nil {
				t.Fatalf("check 2 passed %s; the correlation is not enforced", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("check 2 failed with %v; want the %q leg, not a failure of an earlier leg", err, tc.want)
			}
		})
	}
}
