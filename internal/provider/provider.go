// Package provider is the model-protocol seam (spec §10): one interface the
// executor calls, a name-keyed registry so adding a protocol is one file plus
// one line, and a stub that is the reference implementation the conformance
// suite is proved against.
//
// One policy lives in this package rather than in placement: no_egress is
// re-checked by every adapter's Complete before it sends, because placement is
// a decision made elsewhere and this is the guarantee that has to hold even if a
// code path skips it. Enforcement is a per-adapter call to checkReach plus the
// conformance suite's refusal check (M2 Task 2) — not a structural property of
// the interface, which in Go cannot carry one.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Message is one turn of a conversation. Role is one of system, user,
// assistant or tool. Name is the tool's name on a tool turn and is otherwise
// optional.
//
// ToolCalls holds an assistant turn's emitted calls: the same Call values the
// provider returned in the Response that produced the turn, ids included. The
// endpoint needs the calls themselves, not just their ids, because each family
// makes the assistant replay them (OpenAI assistant.tool_calls, Anthropic
// tool_use blocks). It is empty on every other role.
//
// CallID is a tool turn's correlation id: the Call.ID of the call whose result
// this turn carries, copied from the assistant turn that emitted it. Each family
// has its own field for it (OpenAI tool_call_id, Anthropic tool_use_id), and it
// is what makes a result attributable to the call it answers. It is empty on
// every other role.
//
// A provider must refuse a tool turn whose CallID is empty, or names no call
// carried by a *preceding* assistant turn, with ErrBadRequest: a result that
// answers nothing cannot be attributed to a call, every family rejects it, and a
// refusal at this seam keeps the failure legible instead of turning it into a
// transport round trip. checkToolMessages is that refusal; call it at the top of
// Complete, next to checkReach.
type Message struct {
	Role      string
	Content   string
	Name      string
	ToolCalls []Call
	CallID    string
}

// ToolDef describes a tool the model may call. Schema is the JSON Schema of the
// arguments, passed through byte-for-byte: the three provider APIs differ in
// where they put it, not in what it means.
type ToolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// Call is a tool call the model asked for. ID identifies it for the rest of the
// conversation: the result turn names it in Message.CallID and the assistant
// turn replays the whole call, so an adapter returns it unchanged and never
// invents one. Arguments are the model's own JSON, preserved byte-for-byte so an
// adapter cannot "fix" what the model wrote.
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
	// local and its own configured reach is too, which is spec §10's no_egress
	// guarantee at the provider layer.
	RequireLocal bool
	Reach        string
}

// Usage is what the provider reported for one request. A successful Complete
// must report positive In and Out: the budget reserves against the estimate
// before sending and settles against this afterwards, and cost accounting reads
// it for every run, so "no usage reported" is not a case this seam accepts. A
// provider that cannot obtain the counts on a text response is broken, not free;
// the conformance suite's check 1 refuses a zero, and only a failed Complete may
// return an empty Usage.
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
// estimate.go documents the default's contract, its biases and the invariants
// an override must keep.
//
// Complete must call checkReach before it touches the network, and must refuse
// a tool turn that cannot be attributed (an empty or unknown Message.CallID)
// with ErrBadRequest (checkToolMessages) rather than send an unattributable
// result. Nothing in Go can make an implementation call either, so both are
// documented requirements plus the conformance suite's checks — and every
// adapter must pass the suite (M2 Task 2).
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

// checkToolMessages is the provider layer's tool-correlation rule, and the one
// implementation of it: a tool turn's Message.CallID must name a call carried by
// a *preceding* assistant turn in the same request. An empty id names nothing and
// an id no earlier turn carries names nothing either, so both are refused with
// ErrBadRequest before anything is sent.
//
// Every family enforces the same rule at the wire — OpenAI rejects an unknown
// tool_call_id, Anthropic an unknown tool_use_id — so refusing here costs
// nothing and keeps the failure at the seam that caused it instead of turning it
// into a transport round trip. Check 2 requires the observable: an empty id, an
// unknown id and a result whose call has no preceding turn must all fail with
// ErrBadRequest. At this seam a local refusal and an endpoint's 400 are the same
// error class, so the helper is what makes an adapter's refusal local and
// retry-free; an adapter that only rejects empty strings fails the unknown-id
// leg. It checks only the caller's request: a provider that returns a Call with
// an empty ID is caught by check 2's response assertions instead.
//
// A call carried with an empty ID is not a match — it cannot be answered, and
// indexing "" would make the empty-CallID refusal look satisfied. Only a
// *preceding* assistant turn counts, so a result cannot answer a call the
// conversation has not made yet.
func checkToolMessages(req Request) error {
	calls := make(map[string]bool)
	for i, m := range req.Messages {
		if m.Role == "assistant" {
			for _, c := range m.ToolCalls {
				if c.ID != "" {
					calls[c.ID] = true
				}
			}
			continue
		}
		if m.Role != "tool" {
			continue
		}
		if m.CallID == "" {
			return fmt.Errorf("%w: message %d is a tool turn with no call id, so its result cannot be attributed to the call it answers", ErrBadRequest, i)
		}
		if !calls[m.CallID] {
			return fmt.Errorf("%w: message %d is a tool turn answering call %q, which no preceding assistant turn carries", ErrBadRequest, i, m.CallID)
		}
	}
	return nil
}

// checkReach is the provider layer's no_egress guarantee: the last place a
// request can be stopped before it leaves the machine. Placement already refuses
// wan for no_egress items, but placement is a decision made elsewhere — this is
// the check that holds even if a code path skips it (spec §9: "it lives in the
// adapter so it cannot be bypassed by a code path that skips placement").
//
// Both declarations are checked: the request's (what the caller believes about
// the endpoint) and the adapter's own configured Reach (what the endpoint row
// said when it was built). Any non-local declaration refuses. So does an
// adapter built without a reach: a reach that cannot be verified is not local,
// and a request that merely claims host is not evidence that the endpoint the
// process would reach is local. That is deliberately fail-closed — an adapter
// that forgot to carry Config.Reach fails no_egress work instead of trusting the
// caller (M2 Task 2).
func checkReach(req Request, endpointReach string) error {
	if !req.RequireLocal {
		return nil
	}
	// The adapter's own declaration is authoritative, and the endpoint's reach
	// is a NOT NULL column, so an adapter without one is a wiring bug.
	if endpointReach == "" {
		return fmt.Errorf("%w: the provider was built without an endpoint reach", ErrEgressRefused)
	}
	// Exact match, deliberately: "WAN", " wan" and "wan " are not local either,
	// and guessing that a non-canonical value means host/lan is how a no_egress
	// run leaves the machine.
	if endpointReach != "host" && endpointReach != "lan" {
		return fmt.Errorf("%w: endpoint reach is %q", ErrEgressRefused, endpointReach)
	}
	// The request's declaration is checked too when the caller supplied one. A
	// request that says wan is not overridden by a local endpoint row, because
	// either record may be the stale one; a request that says nothing defers to
	// the endpoint's declaration.
	if req.Reach != "" && req.Reach != "host" && req.Reach != "lan" {
		return fmt.Errorf("%w: request reach is %q", ErrEgressRefused, req.Reach)
	}
	return nil
}

// CheckBaseURL reports whether raw is a base URL the named family could use:
// scheme http or https, a host, and no query or fragment. It exists for the
// board's admin write path, which must refuse an endpoint row no adapter could
// ever construct — endpoints have no update route, so a URL stored blind is
// permanent short of raw SQL.
//
// It validates without normalising, and normalizeAPIBaseURL shares its parse, so
// "what a base URL may be" has one implementation: the write path and the
// adapters cannot drift apart, and a new family inherits the rule by using the
// same helper.
func CheckBaseURL(raw, family string) error {
	_, err := parseAPIBaseURL(raw, family)
	return err
}

// normalizeAPIBaseURL validates an endpoint root and reduces it to the API root
// that a family's paths (openAIChatPath, anthropicMessagesPath, …) are appended
// to. It lives here, beside checkReach and checkToolMessages, because it is the
// same plumbing for every HTTP adapter: two copies of "what a base URL is" is two
// copies free to disagree about which spellings name the same endpoint.
//
// Trailing slashes and a trailing /v1 are dropped so "http://host:8080",
// "http://host:8080/" and "http://host:8080/v1" are one endpoint rather than three
// spellings that produce two wrong URLs. Only the URL's own shape is normalized; a
// path prefix (a reverse proxy serving ".../openai/") is preserved.
//
// A query or fragment is refused: appending a path to a URL that already has one
// would silently produce something that is neither, and no endpoint in this
// project's families needs it. family names the protocol in the error, so a bad
// endpoint row says which adapter refused it.
func normalizeAPIBaseURL(raw, family string) (string, error) {
	u, err := parseAPIBaseURL(raw, family)
	if err != nil {
		return "", err
	}
	path := strings.TrimRight(u.Path, "/")
	path = strings.TrimSuffix(path, "/v1")
	path = strings.TrimRight(path, "/")
	u.Path = path
	u.RawPath = ""
	return u.String(), nil
}

// parseAPIBaseURL is the rule both callers apply; they differ only in whether
// they normalise the result (CheckBaseURL must not, because the board stores
// what the operator typed).
func parseAPIBaseURL(raw, family string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("provider: %s: no base URL in config", family)
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("provider: %s: base URL %q: %w", family, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("provider: %s: base URL %q: scheme is %q, want http or https", family, raw, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("provider: %s: base URL %q: no host", family, raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("provider: %s: base URL %q: must not carry a query or fragment", family, raw)
	}
	return u, nil
}
