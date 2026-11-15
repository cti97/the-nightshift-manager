package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrScriptExhausted is returned by the stub when Complete is called more times
// than the script has entries. It is deliberate: a stub that quietly returned
// an empty response would let a test pass on work that was never modelled.
var ErrScriptExhausted = errors.New("provider: stub script exhausted")

// Stub is the reference Provider (spec §10): it replays pre-scripted responses
// in order, with no network and no key. The conformance suite is proved against
// it, so its behaviour is the floor every real adapter must meet — including
// the reach check that makes no_egress true.
//
// It refuses rather than invents. An exhausted script and a local-only request
// on a non-local endpoint are both errors, never a plausible-looking empty
// response: a stub that guessed would make a broken test look green.
type Stub struct {
	// Reach is the endpoint's declared reach class (Config.Reach). Set it
	// before the first Complete; it is deliberately unguarded, like the rest of
	// a provider's configuration, because changing it mid-run would mean the
	// endpoint the process is talking to changed underneath it.
	Reach string

	mu     sync.Mutex
	script []Response
	next   int
}

// NewStub returns a stub that replays script in order. The slice and its
// tool-call arguments are copied, so a caller that keeps a handle on the script
// it passed in cannot change what has already been scheduled.
func NewStub(script []Response) *Stub {
	return &Stub{script: cloneScript(script)}
}

// cloneScript deep-copies a script, including Call.Arguments, so the stub's
// schedule is immutable from the caller's point of view.
func cloneScript(script []Response) []Response {
	if script == nil {
		return nil
	}
	out := make([]Response, len(script))
	for i, resp := range script {
		out[i] = resp
		if resp.ToolCalls == nil {
			continue
		}
		calls := make([]Call, len(resp.ToolCalls))
		for j, c := range resp.ToolCalls {
			c.Arguments = bytes.Clone(c.Arguments)
			calls[j] = c
		}
		out[i].ToolCalls = calls
	}
	return out
}

func (s *Stub) Name() string { return "stub" }

// Complete replays the next scripted response. The reach, context and tool
// refusals run before a scripted response is spent, exactly as a real adapter
// refuses before it writes the request. The tool refusal is checkToolMessages,
// the same shared rule every adapter calls — not a stub-only reimplementation of
// it. A conformance failure model is answered with its taxonomy sentinel, also
// without spending a response, because the stub is the provider-side end of the
// suite's fixture contract (see conformanceFailure).
func (s *Stub) Complete(ctx context.Context, req Request) (Response, error) {
	if err := checkReach(req, s.Reach); err != nil {
		return Response{}, err
	}
	if err := ctx.Err(); err != nil {
		return Response{}, fmt.Errorf("provider: stub: %w", err)
	}
	if err := checkToolMessages(req); err != nil {
		return Response{}, err
	}
	if err := conformanceFailure(req.Model); err != nil {
		return Response{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next >= len(s.script) {
		return Response{}, fmt.Errorf("%w: %d response(s) already replayed", ErrScriptExhausted, len(s.script))
	}
	resp := s.script[s.next]
	s.next++
	return resp, nil
}

// conformanceFailure maps the conformance suite's reserved failure models to the
// taxonomy error a real endpoint would produce for them. The stub is the
// provider-side end of the suite's fixture contract: an HTTP adapter's fake
// server turns these models into 401/429/400/500 responses and the adapter maps
// them, while the stub has no transport for a fake server to script, so the
// failure is answered here.
func conformanceFailure(model string) error {
	switch model {
	case ConformanceModelAuth:
		return ErrAuth
	case ConformanceModelRateLimited:
		return ErrRateLimited
	case ConformanceModelContextOverflow:
		return ErrContextOverflow
	case ConformanceModelServerError:
		return ErrServer
	}
	return nil
}

// EstimateTokens returns the package's documented default estimate (estimate.go).
// The stub has no tokenizer to be more precise with, and a second heuristic here
// would give the budget two numbers to disagree about.
func (s *Stub) EstimateTokens(req Request) int { return estimateTokens(req) }

// Probe reports ok while the context is live. There is no endpoint behind a
// stub to fail, and reporting a fabricated "warming" or "down" would be
// behaviour a real adapter cannot reproduce. A cancelled probe determines no
// state and says so with an error.
func (s *Stub) Probe(ctx context.Context) (Health, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("provider: stub probe: %w", err)
	}
	return HealthOK, nil
}

// newStubProvider is the registry constructor for "stub". It builds an
// unscripted stub — a script is per-test data, so NewStub is how a test supplies
// one — but carries the endpoint's declared reach, which is the half of the
// no_egress check a caller cannot supply.
func newStubProvider(cfg Config) (Provider, error) {
	s := NewStub(nil)
	s.Reach = cfg.Reach
	return s, nil
}
