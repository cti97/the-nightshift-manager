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

// Complete replays the next scripted response. The reach check runs first and
// consumes nothing, exactly as a real adapter refuses before it writes the
// request, and a cancelled context is refused before a scripted response is
// spent.
func (s *Stub) Complete(ctx context.Context, req Request) (Response, error) {
	if err := checkReach(req, s.Reach); err != nil {
		return Response{}, err
	}
	if err := ctx.Err(); err != nil {
		return Response{}, fmt.Errorf("provider: stub: %w", err)
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

// EstimateTokens is the package's default input-token heuristic: about four
// bytes per token, plus a fixed overhead per chat turn and per tool definition.
// It is additive by construction, so growing any field can never lower the
// result — the property the budget channel relies on when it reserves at 1.2×
// before sending. It counts input only: MaxTokens caps the reply and is not
// sent input.
//
// Every adapter starts from this; a family with a real tokenizer may override
// it (M2 Task 7 owns the documented contract).
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

const (
	// bytesPerToken is the default heuristic's ratio. English prose runs a
	// little over four bytes per token for the families this project talks to;
	// the ceiling division errs high, and the budget reserves above the result.
	bytesPerToken = 4
	// messageOverhead is the framing a chat template adds per turn: the role
	// token and the separators around the content.
	messageOverhead = 4
	// toolOverhead is the framing around one tool definition, separate from the
	// bytes of its name, description and schema.
	toolOverhead = 8
)

// estimateTokens is the default heuristic every provider starts from. Each
// field contributes a non-negative amount and the parts are summed, so making
// any input longer cannot make the estimate smaller — there is no rounding
// across fields that a longer request could tip the other way.
func estimateTokens(req Request) int {
	tokens := textTokens(req.System)
	for _, m := range req.Messages {
		tokens += messageOverhead
		tokens += textTokens(m.Role)
		tokens += textTokens(m.Name)
		tokens += textTokens(m.Content)
	}
	for _, t := range req.Tools {
		tokens += toolOverhead
		tokens += textTokens(t.Name)
		tokens += textTokens(t.Description)
		tokens += textTokens(string(t.Schema))
	}
	if tokens == 0 && (req.System != "" || len(req.Messages) > 0 || len(req.Tools) > 0) {
		// A non-empty request costs at least one token to put on the wire.
		return 1
	}
	return tokens
}

// textTokens is a ceiling division: an 8-byte field is two tokens, not one.
func textTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + bytesPerToken - 1) / bytesPerToken
}
