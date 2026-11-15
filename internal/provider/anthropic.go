package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The Anthropic adapter: the Messages API, with tool use.
//
// It shares nothing with the OpenAI-shaped adapter except the seam and the shared
// refusals, and that is the point of a protocol family: the wire shapes differ
// everywhere the OpenAI one is convenient.
//
//	Request.System      → the top-level "system" field, never a message
//	Message.Role "user" → a user message whose content is one text block
//	Message.ToolCalls   → assistant content blocks, one tool_use per call:
//	                      {"type":"tool_use","id","name","input"}
//	Message.Role "tool" → a *user* message with one tool_result block per result:
//	                      {"type":"tool_result","tool_use_id","content"}
//	ToolDef.Schema      → tools[].input_schema (not function.parameters)
//	Response.content    → text blocks concatenated into Content, tool_use blocks
//	                      into Response.ToolCalls
//	Response.usage      → Response.Usage
//
// Three decisions are documented where they are made because they are the ones a
// reviewer will want to argue with: the missing-max_tokens refusal
// (validateAnthropicRequest), the refusal of a system-role turn
// (validateAnthropicRequest), and the probe's route and cost (Probe).

const (
	anthropicProviderName = "anthropic"
	anthropicMessagesPath = "/v1/messages"
	anthropicModelsPath   = "/v1/models"

	// anthropicAPIVersion is the required anthropic-version header. It pins the
	// wire shape this file was written against; a newer value is a deliberate
	// decision about the protocol, not a value to guess per request.
	anthropicAPIVersion = "2023-06-01"

	// defaultAnthropicTimeout bounds one request when Config.Timeout is unset. It
	// is generous because a long reply (and a large tool result) can take a while;
	// a caller that knows better sets Config.Timeout.
	defaultAnthropicTimeout = 120 * time.Second

	// maxAnthropicResponseBytes caps how much of a response body is read. A
	// completion with a large tool_use input is comfortably under this; an
	// endpoint streaming an unbounded body is not allowed to exhaust memory.
	maxAnthropicResponseBytes = 8 << 20

	// anthropicDefaultSchema is what a ToolDef without a Schema is sent as.
	// input_schema is required and must be a JSON Schema object; an empty schema
	// is the honest description of "any arguments".
	anthropicDefaultSchema = `{"type":"object","properties":{}}`
)

// Anthropic is the Messages API adapter. It is constructed once per endpoint and
// is safe for concurrent use: every field is set by NewAnthropic and never
// mutated afterwards, and the http.Client is safe to share.
type Anthropic struct {
	baseURL string
	apiKey  string
	client  *http.Client
	timeout time.Duration
	// reach is the endpoint's declared reach class from Config.Reach. It is the
	// adapter's half of checkReach: an adapter built without one refuses
	// local-only work rather than trusting the caller's claim.
	reach string
}

// NewAnthropic builds the adapter for the endpoint in cfg. The base URL is the
// API root; a trailing slash and a trailing /v1 are both accepted, because every
// Anthropic document writes the root both ways and both spellings name the same
// endpoint (normalizeAPIBaseURL is the one implementation of that rule).
//
// An empty or unaddressable base URL is an error here rather than a transport
// failure on every call: the endpoint row is wrong, and no request can make it
// right. The API key is optional at this layer — a local proxy in front of the
// API may terminate authentication — and when it is empty no x-api-key header is
// sent at all.
//
// Config.HTTPClient, when set, is used as given; the per-request timeout is still
// applied through the context, so the caller's client cannot make a request run
// forever. Config.Reach is carried for checkReach and is never defaulted: an
// unset reach is fail-closed by design.
func NewAnthropic(cfg Config) (*Anthropic, error) {
	baseURL, err := normalizeAPIBaseURL(cfg.BaseURL, anthropicProviderName)
	if err != nil {
		return nil, err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultAnthropicTimeout
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	return &Anthropic{
		baseURL: baseURL,
		apiKey:  cfg.APIKey,
		client:  client,
		timeout: timeout,
		reach:   cfg.Reach,
	}, nil
}

// The adapter is the interface, checked at compile time rather than discovered
// when a registry construction returns a value dispatch cannot use.
var _ Provider = (*Anthropic)(nil)

func (a *Anthropic) Name() string { return anthropicProviderName }

// newAnthropicProvider is the registry constructor for "anthropic": one protocol
// is one file plus one registry line.
func newAnthropicProvider(cfg Config) (Provider, error) { return NewAnthropic(cfg) }

// Complete sends one Messages API request. The two shared refusals run first, in
// the same order as every other adapter: checkReach (no_egress) and
// checkToolMessages (a tool result must answer a call a preceding assistant turn
// made). Both are local, as is this adapter's own validation of the request
// shape, so a request that cannot be sent is refused before anything is built;
// the fake servers in the tests assert the request count stays at zero.
func (a *Anthropic) Complete(ctx context.Context, req Request) (Response, error) {
	if err := checkReach(req, a.reach); err != nil {
		return Response{}, err
	}
	if err := checkToolMessages(req); err != nil {
		return Response{}, err
	}
	if err := validateAnthropicRequest(req); err != nil {
		return Response{}, err
	}
	if err := ctx.Err(); err != nil {
		return Response{}, fmt.Errorf("provider: anthropic: %w", err)
	}

	payload, err := marshalAnthropicRequest(req)
	if err != nil {
		return Response{}, err
	}
	status, body, err := a.do(ctx, http.MethodPost, anthropicMessagesPath, payload)
	if err != nil {
		return Response{}, err
	}
	if status < http.StatusOK || status > 299 {
		return Response{}, anthropicHTTPError(status, body, req.Model)
	}
	return decodeAnthropicResponse(body, req.Model)
}

// EstimateTokens is the package's shared input-token heuristic: about four bytes
// per token, plus a fixed overhead per chat turn and per tool definition. There
// is no tokenizer in this package and no dependency is allowed, so inventing a
// second heuristic for this family would buy nothing but a second number for the
// budget to disagree with itself about.
func (a *Anthropic) EstimateTokens(req Request) int { return estimateTokens(req) }

// Probe asks the endpoint for its model list. The four health states are the
// answer to "what should an operator do", and only an answer can determine one:
//
//	2xx            → ok, when the body is a model list
//	401, 403       → auth_failed: reachable, key is wrong; fix the key
//	5xx (incl. 529) → down: the server answered and said it is not serving
//	404            → no state, ErrModelMissing: the route is not there, so the
//	                 base URL points at something that is not this protocol
//	429, other 4xx → no state, the taxonomy sentinel: reachable but not usable,
//	                 and "down" would be a lie about a server that answered
//	no answer      → no state, ErrTransport: nothing was determined
//
// What it costs: one authenticated GET /v1/models. That route bills no tokens
// and loads no model, which is why it is the probe rather than a one-token
// completion canary: a canary would spend money on every probe of a metered
// endpoint, and a probe that costs money is a probe an operator turns off. The
// price of the cheaper route is stated plainly: it proves the credential and the
// route, not that the account has quota for the named model — Config carries no
// model name, so a model-aware canary belongs to the endpoint row (Tasks 8/9),
// exactly as it does for the OpenAI adapter.
//
// The 404-versus-transport distinction is the one an operator needs: a wrong base
// URL is a configuration error to fix once, while an unreachable server is a
// machine to wait for. A probe that said ok to any 200 would also turn a web page
// into a healthy endpoint, so a 2xx must decode as a model list.
func (a *Anthropic) Probe(ctx context.Context) (Health, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("provider: anthropic probe: %w", err)
	}
	status, body, err := a.do(ctx, http.MethodGet, anthropicModelsPath, nil)
	if err != nil {
		return "", err
	}
	switch {
	case status >= http.StatusOK && status <= 299:
		if !isModelsList(body) {
			return "", fmt.Errorf("%w: provider: anthropic probe: the models endpoint answered HTTP %d with something that is not a model list", ErrServer, status)
		}
		return HealthOK, nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return HealthAuthFailed, nil
	case status >= 500:
		return HealthDown, nil
	default:
		where := fmt.Sprintf("provider: anthropic probe: HTTP %d", status)
		if detail := anthropicErrorDetail(body); detail != "" {
			where += ": " + detail
		}
		return "", fmt.Errorf("%w: %s", classifyAnthropicStatus(status, body), where)
	}
}

// do issues one request and returns the status and body. A non-2xx status is a
// successful round trip, not an error here: Complete classifies it into the
// taxonomy and Probe reads it as a health state. The error return is reserved for
// "no answer arrived", and it is single-valued: when the context ended the
// request, the context's own error is what comes back, never ErrTransport.
// ErrTransport therefore means exactly "the transport failed on its own", which is
// the contract the OpenAI adapter established and the retry policy depends on.
// Both observation points are guarded — the round trip and the body read — because
// a cancellation can arrive at either.
//
// The per-request timeout is applied with context.WithTimeout rather than the
// client's own Timeout field so it also bounds a caller-supplied HTTPClient, and
// so the failure is a context error the caller can see. A caller's earlier
// deadline wins.
func (a *Anthropic) do(ctx context.Context, method, path string, payload []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, body)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: cannot build the %s %s request: %v", ErrTransport, method, path, err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", anthropicAPIVersion)
	if a.apiKey != "" {
		req.Header.Set("x-api-key", a.apiKey)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, nil, fmt.Errorf("provider: anthropic: %w", ctxErr)
		}
		return 0, nil, fmt.Errorf("%w: %w", ErrTransport, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnthropicResponseBytes))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, nil, fmt.Errorf("provider: anthropic: %w", ctxErr)
		}
		return 0, nil, fmt.Errorf("%w: reading the response body: %w", ErrTransport, err)
	}
	return resp.StatusCode, data, nil
}

// validateAnthropicRequest refuses, before anything is built, a request whose
// shape this protocol cannot carry. The shared checkToolMessages owns the tool
// correlation rule; this covers the rest, and each case is the caller's data, so
// each is ErrBadRequest rather than a server error that would be retried.
//
// Two refusals are decisions rather than mechanics:
//
//  1. max_tokens is required on every request, and a request that sets none is
//     refused rather than defaulted. Defaulting would make the adapter, not the
//     caller, decide how much a step may spend on output — and the budget's
//     ceiling is a safety mechanism, not a suggestion. A low default truncates
//     the reply silently (stop_reason "max_tokens"), possibly mid-tool-call,
//     which is the silent-error class this project refuses; a high one silently
//     spends what the caller did not authorise. The conformance suite supplies
//     MaxTokens itself for exactly this reason. The cost of the refusal is
//     stated: a caller that leaves MaxTokens at zero fails loudly here instead of
//     quietly getting a cap it did not choose.
//
//  2. A system-role message is refused, not moved. This protocol carries one
//     system prompt, outside the conversation. A system-role turn has nowhere to
//     go: emitting it as a message is a 400 from the API, and merging it into
//     Request.System would silently concatenate two prompts the caller wrote for
//     different reasons. The refusal is what keeps "system appears once" true
//     even when the caller supplies it twice.
func validateAnthropicRequest(req Request) error {
	if strings.TrimSpace(req.Model) == "" {
		return fmt.Errorf("%w: the request names no model", ErrBadRequest)
	}
	if req.MaxTokens == 0 {
		return fmt.Errorf("%w: the request sets no MaxTokens; the Messages API requires max_tokens on every request, and this adapter will not invent a reply cap that decides how much the step may spend", ErrBadRequest)
	}
	if req.MaxTokens < 0 {
		return fmt.Errorf("%w: MaxTokens is %d; max_tokens must be positive, and a negative reply cap is not a cap", ErrBadRequest, req.MaxTokens)
	}
	for i, m := range req.Messages {
		switch m.Role {
		case "user", "assistant", "tool":
		case "system":
			return fmt.Errorf("%w: message %d is a system turn; this protocol carries the system prompt in the request's top-level system field, so a system turn has nowhere to go (merging it with Request.System would silently change the prompt the caller wrote)", ErrBadRequest, i)
		default:
			return fmt.Errorf("%w: message %d has role %q, which is not a Messages API role", ErrBadRequest, i, m.Role)
		}
		if len(m.ToolCalls) > 0 && m.Role != "assistant" {
			return fmt.Errorf("%w: message %d is a %s turn carrying tool calls; only an assistant turn emits them", ErrBadRequest, i, m.Role)
		}
		if m.Role != "tool" && m.Content == "" && len(m.ToolCalls) == 0 {
			return fmt.Errorf("%w: message %d is a %s turn with no content and no tool call; this protocol has no empty turn", ErrBadRequest, i, m.Role)
		}
		for j, c := range m.ToolCalls {
			if c.ID == "" {
				return fmt.Errorf("%w: message %d call %d (%s) has no id, so a tool result could never be attributed to it", ErrBadRequest, i, j, c.Name)
			}
			if strings.TrimSpace(c.Name) == "" {
				return fmt.Errorf("%w: message %d call %d has no name", ErrBadRequest, i, j)
			}
			if err := checkAnthropicArguments(c.Arguments); err != nil {
				return fmt.Errorf("%w: message %d call %d (%s): %v", ErrBadRequest, i, j, c.Name, err)
			}
		}
	}
	for i, t := range req.Tools {
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("%w: tool %d has no name", ErrBadRequest, i)
		}
		if len(t.Schema) > 0 {
			if !json.Valid(t.Schema) {
				return fmt.Errorf("%w: tool %q has a schema that is not valid JSON", ErrBadRequest, t.Name)
			}
			// input_schema must be a JSON Schema object; a bare string or array
			// is refused here rather than by the endpoint, because it is the
			// caller's data and no round trip can make it valid.
			if trimmed := bytes.TrimSpace(t.Schema); len(trimmed) == 0 || trimmed[0] != '{' {
				return fmt.Errorf("%w: tool %q has a schema that is not a JSON object", ErrBadRequest, t.Name)
			}
		}
	}
	return nil
}

// checkAnthropicArguments is the one place replay differs from the OpenAI
// adapter, and the difference is the protocol's, not a preference. There the
// arguments travel as a JSON *string*, so a malformed one is echoed and the tool
// layer turns it into an answerable tool error. Here the field is a JSON
// *object*: bytes that are not JSON cannot be represented at all, so a caller
// that supplies them is refused locally with ErrBadRequest instead of being
// silently "repaired" into {} (which would hide what the model wrote) or sent as
// an invalid envelope (which every server rejects anyway). An absent argument set
// is not malformed: it is the empty object, this protocol's spelling of a
// no-argument call, and it is filled in at marshal time.
func checkAnthropicArguments(arguments json.RawMessage) error {
	trimmed := bytes.TrimSpace(arguments)
	if len(trimmed) == 0 {
		return nil
	}
	if !json.Valid(trimmed) {
		return fmt.Errorf("arguments are not valid JSON, and this protocol carries them as a JSON object with nowhere to preserve text that is not JSON")
	}
	if trimmed[0] != '{' {
		return fmt.Errorf("arguments are %s, not a JSON object", trimmed)
	}
	return nil
}

// --- the request wire shape ---

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system,omitempty"`
	Messages  []anthropicMessage `json:"messages"`
	Tools     []anthropicTool    `json:"tools,omitempty"`
	// ToolChoice is an object here, not OpenAI's "auto" string.
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
}

// anthropicMessage is one turn. Content is always the block array: this adapter
// never uses the string shorthand, so one shape covers text, calls and results.
type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock is one content block of any of the three kinds this seam sends.
// The fields are shared and omitempty per kind; a block carries exactly the ones
// its type names.
type anthropicBlock struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result. Content is a pointer so an empty result is sent as "" rather
	// than dropped: content is required on the block, and a result the tool layer
	// produced empty is still a result.
	ToolUseID string  `json:"tool_use_id,omitempty"`
	Content   *string `json:"content,omitempty"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
}

// marshalAnthropicRequest translates the seam's Request into the wire. It runs
// after validateAnthropicRequest, so the only error left is a tool schema
// RawMessage that encoding/json refuses — which is still the caller's data and is
// reported as ErrBadRequest rather than as a transport failure.
func marshalAnthropicRequest(req Request) ([]byte, error) {
	wire := anthropicRequest{Model: req.Model, MaxTokens: req.MaxTokens, System: req.System}
	for i := 0; i < len(req.Messages); {
		m := req.Messages[i]
		if m.Role != "tool" {
			wire.Messages = append(wire.Messages, anthropicMessageFor(m))
			i++
			continue
		}
		// Consecutive tool turns become one user turn with one tool_result block
		// per call. Every result for an assistant turn must arrive in the single
		// user message that follows it, and splitting them into consecutive user
		// messages is at best an implicit merge and at worst a 400. Merging here
		// is a translation of the seam's one-result-per-turn shape, not a change
		// to it: the ids still identify each result.
		blocks := make([]anthropicBlock, 0, 1)
		for i < len(req.Messages) && req.Messages[i].Role == "tool" {
			t := req.Messages[i]
			content := t.Content
			blocks = append(blocks, anthropicBlock{Type: "tool_result", ToolUseID: t.CallID, Content: &content})
			i++
		}
		wire.Messages = append(wire.Messages, anthropicMessage{Role: "user", Content: blocks})
	}
	for _, t := range req.Tools {
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(anthropicDefaultSchema)
		}
		wire.Tools = append(wire.Tools, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	if len(wire.Tools) > 0 {
		// "auto" is the protocol's default, but sending it makes the choice
		// explicit on the wire: a server that defaults differently is then told
		// what this project wants.
		wire.ToolChoice = &anthropicToolChoice{Type: "auto"}
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot encode the request: %v", ErrBadRequest, err)
	}
	return body, nil
}

// anthropicMessageFor translates one user or assistant turn. A text block is
// emitted only when there is text: an assistant turn that carries only calls has
// no text block, and an empty text block is refused by the API.
func anthropicMessageFor(m Message) anthropicMessage {
	msg := anthropicMessage{Role: m.Role}
	if m.Content != "" {
		msg.Content = append(msg.Content, anthropicBlock{Type: "text", Text: m.Content})
	}
	for _, c := range m.ToolCalls {
		input := c.Arguments
		if len(bytes.TrimSpace(input)) == 0 {
			input = json.RawMessage(`{}`)
		}
		msg.Content = append(msg.Content, anthropicBlock{Type: "tool_use", ID: c.ID, Name: c.Name, Input: input})
	}
	return msg
}

// --- the response wire shape ---

type anthropicWireResponse struct {
	Content    []anthropicWireBlock `json:"content"`
	StopReason *string              `json:"stop_reason"`
	Usage      *anthropicWireUsage  `json:"usage"`
}

type anthropicWireBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	ID   string `json:"id"`
	Name string `json:"name"`
	// Input is kept raw: it is the model's own JSON, and decoding it into a map
	// would reorder the keys the tool layer will hand to json.Unmarshal.
	Input json.RawMessage `json:"input"`
}

type anthropicWireUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// decodeAnthropicResponse maps the content blocks and the usage onto the seam.
//
// Text blocks are concatenated in the order they arrived. A block type this seam
// does not model — a thinking block today, a future server-side tool block — is
// skipped: refusing it would make the adapter unusable against any model that
// thinks out loud, and guessing that it is reply text would put words in the
// model's mouth. A tool_use block is the exception that must be understood: one
// without an id or a name could never be answered, so it is ErrServer rather than
// a Call with an empty field that fails later, at replay.
//
// A tool_use input is passed through byte-for-byte. The wire field is a JSON
// object, so an input that is not one means the endpoint is not speaking this
// protocol, which is ErrServer; the input is never "fixed".
//
// Usage is required to be countable and positive on every successful completion,
// for the reason the OpenAI adapter documents: the budget settles against Usage
// and cost accounting reads it for every run, so substituting an estimate would
// put a number the adapter made up into a field that looks measured.
func decodeAnthropicResponse(body []byte, model string) (Response, error) {
	var wire anthropicWireResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return Response{}, fmt.Errorf("%w: model %q: the response body is not a Messages API message: %v", ErrServer, model, err)
	}
	if len(wire.Content) == 0 {
		return Response{}, fmt.Errorf("%w: model %q: the response carried no content blocks", ErrServer, model)
	}
	resp := Response{}
	if wire.StopReason != nil {
		resp.Stop = *wire.StopReason
	}
	for i, block := range wire.Content {
		switch block.Type {
		case "text":
			resp.Content += block.Text
		case "tool_use":
			if block.ID == "" {
				return Response{}, fmt.Errorf("%w: model %q: content block %d is a tool_use with no id, so its result could never be correlated with it", ErrServer, model, i)
			}
			if strings.TrimSpace(block.Name) == "" {
				return Response{}, fmt.Errorf("%w: model %q: content block %d is a tool_use with no name", ErrServer, model, i)
			}
			input := bytes.TrimSpace(block.Input)
			if len(input) == 0 || input[0] != '{' {
				return Response{}, fmt.Errorf("%w: model %q: content block %d (%s) carries an input that is not a JSON object: %s", ErrServer, model, i, block.Name, block.Input)
			}
			resp.ToolCalls = append(resp.ToolCalls, Call{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: append(json.RawMessage(nil), input...),
			})
		default:
			// See the doc comment: an unmodelled block is skipped deliberately.
		}
	}
	if resp.Content == "" && len(resp.ToolCalls) == 0 {
		return Response{}, fmt.Errorf("%w: model %q: the response carried no text and no tool use; there is nothing to report or act on", ErrServer, model)
	}

	if wire.Usage == nil {
		return Response{}, fmt.Errorf("%w: model %q: the endpoint reported no usage; a completion the budget cannot settle against is not a success", ErrServer, model)
	}
	resp.Usage = Usage{In: wire.Usage.InputTokens, Out: wire.Usage.OutputTokens}
	if resp.Usage.In <= 0 || resp.Usage.Out <= 0 {
		return Response{}, fmt.Errorf("%w: model %q: usage %+v is not countable (input_tokens and output_tokens must both be positive); the budget cannot settle against it",
			ErrServer, model, resp.Usage)
	}
	return resp, nil
}

// --- errors ---

// anthropicErrorEnvelope is this family's error body:
// {"type":"error","error":{"type":...,"message":...}}. Some gateways in front of
// the API return a bare message instead, so every caller tolerates a decode
// failure and falls back to the body text.
type anthropicErrorEnvelope struct {
	Type  string `json:"type"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// anthropicHTTPError classifies a non-2xx answer into the taxonomy, keeping the
// endpoint's own message so the operator sees what it said, and the model name so
// a multi-model endpoint's failure is attributable.
func anthropicHTTPError(status int, body []byte, model string) error {
	class := classifyAnthropicStatus(status, body)
	where := fmt.Sprintf("provider: anthropic: HTTP %d", status)
	if model != "" {
		where = fmt.Sprintf("provider: anthropic: model %q: HTTP %d", model, status)
	}
	if detail := anthropicErrorDetail(body); detail != "" {
		where += ": " + detail
	}
	return fmt.Errorf("%w: %s", class, where)
}

// classifyAnthropicStatus maps a status to one of the seven classes. The status is
// the primary signal because this family uses the same codes for the same things;
// only a 400 is refined by its body, because context overflow is the one class it
// reports as a 400 with recognizable wording.
//
// 403 is auth rather than bad_request: this family uses it for a key that exists
// but cannot use the requested resource, and retrying it is exactly the "burn
// attempts on a bad credential" failure. A 413 is context overflow: the request
// (almost always a large tool result) did not fit, and no retry of the same bytes
// can make it fit. 529 is the family's "overloaded" status and is a 5xx, so it is
// ErrServer — retryable, which is what an overloaded endpoint wants.
func classifyAnthropicStatus(status int, body []byte) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrAuth
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status == http.StatusNotFound:
		return ErrModelMissing
	case status == http.StatusRequestTimeout:
		return ErrTransport
	case status == http.StatusRequestEntityTooLarge:
		return ErrContextOverflow
	case status >= 500:
		return ErrServer
	case status >= 400:
		if isAnthropicContextOverflow(body) {
			return ErrContextOverflow
		}
		return ErrBadRequest
	default:
		return ErrServer
	}
}

// anthropicContextOverflowPhrases are the spellings this family uses for "the
// prompt did not fit". They are matched case-insensitively against the whole body,
// so they work whether the phrase lives in the error's message or its type, and
// whether or not the body is the documented envelope. Only consulted for a 400;
// none of them appears in an ordinary malformed-request body.
var anthropicContextOverflowPhrases = []string{
	"prompt is too long",
	"input length and max_tokens exceed context limit",
	"exceeds the maximum number of tokens",
	"context_length_exceeded",
	"context window",
	"too many tokens",
	"reduce the length",
	"input is too long",
}

func isAnthropicContextOverflow(body []byte) bool {
	text := strings.ToLower(string(body))
	for _, phrase := range anthropicContextOverflowPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// anthropicErrorDetail is the endpoint's own message, with its error type when it
// has one, truncated so a huge HTML error page cannot bury the class. A body that
// is not the documented envelope is used as-is: an operator can still read it, and
// discarding it would leave only "HTTP 400".
func anthropicErrorDetail(body []byte) string {
	var envelope anthropicErrorEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != nil && envelope.Error.Message != "" {
		detail := envelope.Error.Message
		if envelope.Error.Type != "" {
			detail += " (" + envelope.Error.Type + ")"
		}
		return truncateForError(detail)
	}
	return truncateForError(strings.TrimSpace(string(body)))
}
