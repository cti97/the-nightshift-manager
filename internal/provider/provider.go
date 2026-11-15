// Package provider is the model-protocol seam (spec §10): one interface the
// executor calls, a name-keyed registry so adding a protocol is one file plus
// one line, and a stub that is the reference implementation the conformance
// suite is proved against.
//
// One policy lives in this package rather than in placement: no_egress is
// re-checked here, in the path every adapter's request passes through, because
// placement is a decision and this is the guarantee.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Message is one turn of a conversation. Role is one of system, user,
// assistant or tool; Name is the tool's name on a tool turn and is otherwise
// optional.
type Message struct {
	Role    string
	Content string
	Name    string
}

// ToolDef describes a tool the model may call. Schema is the JSON Schema of the
// arguments, passed through byte-for-byte: the three provider APIs differ in
// where they put it, not in what it means.
type ToolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// Call is a tool call the model asked for. Arguments are the model's own JSON,
// preserved byte-for-byte so an adapter cannot "fix" what the model wrote.
type Call struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Request is one completion request.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []ToolDef
	MaxTokens int

	// Reach is the endpoint's declared reach class: host, lan or wan. When
	// RequireLocal is set, a provider refuses to send unless that class is
	// local, which is spec §10's no_egress guarantee at the provider layer.
	RequireLocal bool
	Reach        string
}

// Usage is what the provider reported for one request. Providers that do not
// report usage leave it zero; the budget layer treats that as unknown, not free.
type Usage struct{ In, Out int }

// Response is one completion: either content, or tool calls, or both.
type Response struct {
	Content   string
	ToolCalls []Call
	Usage     Usage
	Stop      string
}

// Health is what Probe reports: a state, not a bool, because the four states
// need different responses from an operator (spec §10). warming is a llama.cpp
// model loading on its first request, which is not a failure worth alerting on.
//
// A non-nil error from Probe means no state was determined — the zero Health
// accompanies it and is not a fifth state.
type Health string

const (
	HealthOK         Health = "ok"
	HealthWarming    Health = "warming"
	HealthDown       Health = "down"
	HealthAuthFailed Health = "auth_failed"
)

// Provider is one model protocol. Complete returns the model's reply or an
// error; EstimateTokens is the input-token estimate the budget reserves
// against before sending, because most APIs report usage only afterwards.
//
// Complete must call checkReach before it touches the network. Nothing in Go
// can make an implementation call a method, so the call is a documented
// requirement plus the conformance suite's refusal check — and every adapter
// must pass the suite (M2 Task 2).
//
// Complete must return promptly when ctx is cancelled and must not leave
// requests running behind the context.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request) (Response, error)
	EstimateTokens(req Request) int
	Probe(ctx context.Context) (Health, error)
}

// ErrEgressRefused is returned when a request requires a local endpoint and the
// endpoint's declared reach is not host or lan. It is deliberately not part of
// the retry taxonomy (M2 Task 6): retrying would send the same bytes again.
var ErrEgressRefused = errors.New("provider: refusing to send to a non-local endpoint")

// checkReach is the provider layer's no_egress guarantee: the last place a
// request can be stopped before it leaves the machine. Placement already refuses
// wan for no_egress items, but placement is a decision made elsewhere — this is
// the check that holds even if a code path skips it (spec §9: "it lives in the
// adapter so it cannot be bypassed by a code path that skips placement").
//
// Both declarations are checked: the request's (what the caller believes about
// the endpoint) and the adapter's own configured Reach (what the endpoint row
// said when it was built). Any non-local declaration refuses, and so does an
// absence of declarations — a reach that cannot be verified is not local.
func checkReach(req Request, endpointReach string) error {
	if !req.RequireLocal {
		return nil
	}
	declared := 0
	for _, reach := range []string{req.Reach, endpointReach} {
		if reach == "" {
			continue
		}
		declared++
		// Exact match, deliberately: "WAN", " wan" and "wan " are not local
		// either, and guessing that a non-canonical value means host/lan is how
		// a no_egress run leaves the machine.
		if reach != "host" && reach != "lan" {
			return fmt.Errorf("%w: endpoint reach is %q", ErrEgressRefused, reach)
		}
	}
	if declared == 0 {
		return fmt.Errorf("%w: endpoint reach is unstated", ErrEgressRefused)
	}
	return nil
}
