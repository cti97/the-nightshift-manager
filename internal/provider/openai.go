package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// The OpenAI-compatible adapter: chat completions with function tools, which is
// one protocol spoken by OpenAI, llama.cpp's llama-server, vLLM, LM Studio, Groq
// and OpenRouter. They differ in base URL, key and model names — nothing else —
// so there is no per-product branch in this file, and a branch on a hostname
// would be a defect rather than a feature.
//
// The wire mapping, which is the whole adapter:
//
//	Request.System              → a leading {"role":"system"} message
//	Message.ToolCalls           → assistant.tool_calls, each
//	                              {"id","type":"function","function":{"name","arguments"}}
//	                              where arguments is the model's JSON *string*
//	Message.CallID              → tool.tool_call_id
//	ToolDef.Schema              → tools[].function.parameters
//	Response.choices[0].message → Response.Content + Response.ToolCalls
//	Response.usage              → Response.Usage
//
// Three decisions are documented where they are made because they are the ones
// a reviewer will want to argue with: the missing-usage refusal
// (decodeOpenAIResponse), the arguments passthrough (decodeOpenAIResponse), and
// the probe's status mapping (Probe).

const (
	openAIProviderName = "openai"
	openAIChatPath     = "/v1/chat/completions"
	openAIModelsPath   = "/v1/models"

	// defaultOpenAITimeout bounds one request when Config.Timeout is unset. It is
	// generous because a local llama.cpp or Ollama-backed server may spend its
	// first request loading a model; a caller that knows better sets Config.Timeout.
	defaultOpenAITimeout = 120 * time.Second

	// maxOpenAIResponseBytes caps how much of a response body is read. A
	// completion with a large tool-call argument is comfortably under this; an
	// endpoint streaming an unbounded body is not allowed to exhaust memory.
	maxOpenAIResponseBytes = 8 << 20

	// openAIDefaultSchema is what a ToolDef without a Schema is sent as. OpenAI
	// requires parameters to be a JSON Schema object, and an empty schema is the
	// honest description of "any arguments"; a missing field would be a 400.
	openAIDefaultSchema = `{"type":"object","properties":{}}`
)

// OpenAI is the chat-completions adapter. It is constructed once per endpoint
// and is safe for concurrent use: every field is set by NewOpenAI and never
// mutated afterwards, and the http.Client is safe to share.
type OpenAI struct {
	baseURL string
	apiKey  string
	client  *http.Client
	timeout time.Duration
	// reach is the endpoint's declared reach class from Config.Reach. It is the
	// adapter's half of checkReach: an adapter built without one refuses
	// local-only work rather than trusting the caller's claim.
	reach string
}

// NewOpenAI builds the adapter for the endpoint in cfg. The base URL is the API
// root; a trailing slash and a trailing /v1 are both accepted, because every
// OpenAI-compatible document writes the root both ways and both spellings name
// the same endpoint.
//
// An empty or unaddressable base URL is an error here rather than a transport
// failure on every call: the endpoint row is wrong, and no request can make it
// right. The API key is optional — a llama.cpp or LM Studio server usually has
// none — and when it is empty no Authorization header is sent at all.
//
// Config.HTTPClient, when set, is used as given (a test's fake transport, or a
// client with a pool or proxy the caller owns); the per-request timeout is still
// applied through the context, so the caller's client cannot make a request run
// forever. Config.Reach is carried for checkReach and is never defaulted: an
// unset reach is fail-closed by design.
func NewOpenAI(cfg Config) (*OpenAI, error) {
	baseURL, err := normalizeOpenAIBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultOpenAITimeout
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	return &OpenAI{
		baseURL: baseURL,
		apiKey:  cfg.APIKey,
		client:  client,
		timeout: timeout,
		reach:   cfg.Reach,
	}, nil
}

// normalizeOpenAIBaseURL validates the endpoint and reduces it to the API root
// that openAIChatPath and openAIModelsPath are appended to. Trailing slashes and
// a trailing /v1 are dropped so "http://host:8080", "http://host:8080/" and
// "http://host:8080/v1" are the same endpoint rather than three spellings that
// produce two wrong URLs. Only the URL's own shape is normalized; a path prefix
// (a reverse proxy serving ".../openai/") is preserved.
//
// A query or fragment is refused: appending a path to a URL that already has one
// would silently produce something that is neither, and no OpenAI-compatible
// endpoint needs it.
func normalizeOpenAIBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("provider: openai: no base URL in config")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("provider: openai: base URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("provider: openai: base URL %q: scheme is %q, want http or https", raw, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("provider: openai: base URL %q: no host", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("provider: openai: base URL %q: must not carry a query or fragment", raw)
	}
	path := strings.TrimRight(u.Path, "/")
	path = strings.TrimSuffix(path, "/v1")
	path = strings.TrimRight(path, "/")
	u.Path = path
	u.RawPath = ""
	return u.String(), nil
}

// The adapter is the interface, checked at compile time rather than discovered
// when a registry construction returns a value dispatch cannot use.
var _ Provider = (*OpenAI)(nil)

func (o *OpenAI) Name() string { return openAIProviderName }

// newOpenAIProvider is the registry constructor for "openai". One protocol is one
// file plus one registry line; this is the function value that line registers,
// kept named so NewRegistry reads as a list of protocols rather than of closures.
func newOpenAIProvider(cfg Config) (Provider, error) { return NewOpenAI(cfg) }

// Complete sends one chat-completions request. The two shared refusals run
// first, in the same order as every other adapter: checkReach (no_egress) and
// checkToolMessages (a tool result must answer a call a preceding assistant turn
// made). Both are local, as is this adapter's own validation of the request
// shape, so a request that cannot be sent is refused before anything is written;
// the fake servers in the tests assert the request count stays at zero.
func (o *OpenAI) Complete(ctx context.Context, req Request) (Response, error) {
	if err := checkReach(req, o.reach); err != nil {
		return Response{}, err
	}
	if err := checkToolMessages(req); err != nil {
		return Response{}, err
	}
	if err := validateOpenAIRequest(req); err != nil {
		return Response{}, err
	}
	if err := ctx.Err(); err != nil {
		return Response{}, fmt.Errorf("provider: openai: %w", err)
	}

	payload, err := marshalOpenAIRequest(req)
	if err != nil {
		return Response{}, err
	}
	status, body, err := o.do(ctx, http.MethodPost, openAIChatPath, payload)
	if err != nil {
		return Response{}, err
	}
	if status < http.StatusOK || status > 299 {
		return Response{}, openAIHTTPError(status, body, req.Model)
	}
	return decodeOpenAIResponse(body, req.Model)
}

// EstimateTokens is the package's shared input-token heuristic: about four
// bytes per token, plus a fixed overhead per chat turn and per tool definition
// (estimateTokens, which Task 7 moves into estimate.go).
// There is no tokenizer in this package and no dependency is allowed, so
// inventing a second heuristic for this family would buy nothing but a second
// number for the budget to disagree with itself about.
//
// It lands inside the conformance suite's ±40% band: the suite's oracle is 4/3
// tokens per word of the prose it placed in the request, and this heuristic runs
// 70 tokens for the one-paragraph request (oracle 60, +16.7%) and 265 for the
// four-paragraph one (oracle 240, +10.4%). The band exists because a real
// tokenizer differs from the word oracle; the estimate errs high on purpose,
// because the budget reserves above it.
func (o *OpenAI) EstimateTokens(req Request) int { return estimateTokens(req) }

// Probe asks the endpoint for its model list. The four health states are the
// answer to "what should an operator do", and only an answer can determine one:
//
//	2xx            → ok, when the body is an OpenAI models list
//	401, 403       → auth_failed: reachable, key is wrong; fix the key
//	5xx            → down: the server answered and said it is not serving
//	404            → no state, ErrModelMissing: the route is not there, so the
//	                 base URL points at something that is not this protocol
//	429, other 4xx → no state, the taxonomy sentinel: reachable but not usable,
//	                 and "down" would be a lie about a server that answered
//	no answer      → no state, ErrTransport: nothing was determined
//
// The 404-versus-transport distinction is the one an operator needs: a wrong base
// URL is a configuration error to fix once, while an unreachable server is a
// machine to wait for. Returning HealthDown for both would turn the first into
// the second, and a probe that says ok to any 200 would turn a web page into a
// healthy endpoint, so a 2xx must also decode as a models list.
//
// Probe lists models because it cannot run a completion canary: Config carries no
// model name, so a canary would have to guess one. An endpoint that serves
// completions but has no /v1/models routes as an error here; every family this
// adapter covers implements the route, and a model-aware canary belongs to the
// endpoint row (Task 8), not to a Config that does not carry one.
func (o *OpenAI) Probe(ctx context.Context) (Health, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("provider: openai probe: %w", err)
	}
	status, body, err := o.do(ctx, http.MethodGet, openAIModelsPath, nil)
	if err != nil {
		return "", err
	}
	switch {
	case status >= http.StatusOK && status <= 299:
		if !isOpenAIModelsList(body) {
			return "", fmt.Errorf("%w: the models endpoint answered HTTP %d with something that is not an OpenAI models list", ErrServer, status)
		}
		return HealthOK, nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return HealthAuthFailed, nil
	case status >= 500:
		return HealthDown, nil
	default:
		return "", openAIHTTPError(status, body, "")
	}
}

// do issues one request and returns the status and body. A non-2xx status is a
// successful round trip, not an error here: Complete classifies it into the
// taxonomy and Probe reads it as a health state. The error return is reserved for
// "no answer arrived" — a transport failure, a body that could not be read, or a
// context that ended the request — so the two callers cannot confuse the two.
//
// The per-request timeout is applied with context.WithTimeout rather than the
// client's own Timeout field so it also bounds a caller-supplied HTTPClient, and
// so the failure is a context error the caller can see. A caller's earlier
// deadline wins.
func (o *OpenAI) do(ctx context.Context, method, path string, payload []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()

	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.baseURL+path, body)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: cannot build the %s %s request: %v", ErrTransport, method, path, err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}

	resp, err := o.client.Do(req)
	if err != nil {
		// Both classes stay reachable: the call failed to reach the endpoint
		// (ErrTransport) and, when the context is what ended it, the caller can
		// still detect the cancellation with errors.Is.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, nil, fmt.Errorf("%w: %w", ErrTransport, ctxErr)
		}
		return 0, nil, fmt.Errorf("%w: %w", ErrTransport, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxOpenAIResponseBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: reading the response body: %w", ErrTransport, err)
	}
	return resp.StatusCode, data, nil
}

// validateOpenAIRequest refuses, before anything is written, a request whose
// shape this protocol cannot carry. The shared checkToolMessages owns the tool
// correlation rule; this covers the rest: an unnamed model, a negative reply cap,
// a role that is not an OpenAI chat role, tool calls on a turn that cannot emit
// them, a tool definition without a name or with a schema that is not JSON, and an
// assistant turn whose arguments cannot be replayed as the wire's JSON string.
// Each is the caller's data, so each is ErrBadRequest rather than a server error
// that would be retried.
func validateOpenAIRequest(req Request) error {
	if strings.TrimSpace(req.Model) == "" {
		return fmt.Errorf("%w: the request names no model", ErrBadRequest)
	}
	if req.MaxTokens < 0 {
		return fmt.Errorf("%w: MaxTokens is %d; a negative reply cap is not a cap", ErrBadRequest, req.MaxTokens)
	}
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "user", "assistant", "tool":
		default:
			return fmt.Errorf("%w: message %d has role %q, which is not an OpenAI chat role", ErrBadRequest, i, m.Role)
		}
		if len(m.ToolCalls) > 0 && m.Role != "assistant" {
			return fmt.Errorf("%w: message %d is a %s turn carrying tool calls; only an assistant turn emits them", ErrBadRequest, i, m.Role)
		}
		for j, c := range m.ToolCalls {
			if !json.Valid(c.Arguments) {
				return fmt.Errorf("%w: message %d call %d (%s) has arguments that are not valid JSON, so they cannot be replayed as the wire's JSON string", ErrBadRequest, i, j, c.Name)
			}
		}
	}
	for i, t := range req.Tools {
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("%w: tool %d has no name", ErrBadRequest, i)
		}
		if len(t.Schema) > 0 && !json.Valid(t.Schema) {
			return fmt.Errorf("%w: tool %q has a schema that is not valid JSON", ErrBadRequest, t.Name)
		}
	}
	return nil
}

// --- the request wire shape ---

type openAIRequest struct {
	Model      string          `json:"model"`
	Messages   []openAIMessage `json:"messages"`
	Tools      []openAITool    `json:"tools,omitempty"`
	ToolChoice string          `json:"tool_choice,omitempty"`
	// MaxTokens is the reply cap under the field every family in this shape
	// accepts. OpenAI's newer max_completion_tokens is not universal (llama.cpp,
	// vLLM and LM Studio all read max_tokens), so the older spelling is the one
	// that works everywhere.
	MaxTokens int `json:"max_tokens,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    *string          `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	// Message.Name is deliberately not sent. A role tool message is identified
	// by tool_call_id; "name" belonged to the deprecated function role and strict
	// OpenAI-compatible servers reject it on a tool turn.
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIFunctionCall struct {
	Name string `json:"name"`
	// Arguments is a JSON string on the wire, not an object: the model's own
	// bytes go inside it and must come back out unchanged.
	Arguments string `json:"arguments"`
}

type openAITool struct {
	Type     string            `json:"type"`
	Function openAIFunctionDef `json:"function"`
}

type openAIFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// marshalOpenAIRequest translates the seam's Request into the wire. It runs after
// validateOpenAIRequest, so the only error left is a RawMessage that encoding/json
// refuses — which is still the caller's data and is reported as ErrBadRequest
// rather than as a transport failure.
func marshalOpenAIRequest(req Request) ([]byte, error) {
	wire := openAIRequest{Model: req.Model, MaxTokens: req.MaxTokens}
	if req.System != "" {
		wire.Messages = append(wire.Messages, openAIMessage{Role: "system", Content: stringPtr(req.System)})
	}
	for _, m := range req.Messages {
		wire.Messages = append(wire.Messages, openAIMessageFor(m))
	}
	for _, t := range req.Tools {
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(openAIDefaultSchema)
		}
		wire.Tools = append(wire.Tools, openAITool{
			Type: "function",
			Function: openAIFunctionDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schema,
			},
		})
	}
	if len(wire.Tools) > 0 {
		// "auto" is the protocol's default, but sending it makes the choice
		// explicit on the wire (and the brief requires tool_choice be handled):
		// a server that defaults differently is then told what this project wants.
		wire.ToolChoice = "auto"
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot encode the request: %v", ErrBadRequest, err)
	}
	return body, nil
}

// openAIMessageFor translates one turn. Content is a pointer so an assistant turn
// that carries only tool calls omits it (OpenAI's own shape is null there) while
// every other turn sends its content, including an empty tool result, which is a
// required field for the tool role.
func openAIMessageFor(m Message) openAIMessage {
	if m.Role == "tool" {
		return openAIMessage{Role: "tool", Content: stringPtr(m.Content), ToolCallID: m.CallID}
	}
	wire := openAIMessage{Role: m.Role}
	if len(m.ToolCalls) > 0 {
		wire.ToolCalls = make([]openAIToolCall, len(m.ToolCalls))
		for i, c := range m.ToolCalls {
			wire.ToolCalls[i] = openAIToolCall{
				ID:       c.ID,
				Type:     "function",
				Function: openAIFunctionCall{Name: c.Name, Arguments: string(c.Arguments)},
			}
		}
	}
	if m.Content != "" || len(wire.ToolCalls) == 0 {
		wire.Content = stringPtr(m.Content)
	}
	return wire
}

func stringPtr(s string) *string { return &s }

// --- the response wire shape ---

type openAIWireResponse struct {
	Choices []openAIWireChoice `json:"choices"`
	Usage   *openAIWireUsage   `json:"usage"`
}

type openAIWireChoice struct {
	Message      openAIWireResponseMessage `json:"message"`
	FinishReason *string                   `json:"finish_reason"`
}

type openAIWireResponseMessage struct {
	Content   *string              `json:"content"`
	ToolCalls []openAIWireToolCall `json:"tool_calls"`
}

type openAIWireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
		// Arguments arrives as a JSON string, so it is kept raw here and
		// unquoted once, in decodeOpenAIResponse: decoding it into a map would
		// reorder the model's keys.
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type openAIWireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// decodeOpenAIResponse maps the first choice and the usage onto the seam.
//
// Arguments are unquoted — the wire's string is the model's JSON text — and never
// decoded into a map: the tool layer hands these bytes to json.Unmarshal, and a
// map round trip would reorder keys and break a call that names its arguments in
// the model's order. A string that is not itself valid JSON is passed through
// unchanged: it is the model's own bad output, the tool layer turns it into a
// tool error the model can answer, and "fixing" it into {} would hide the model's
// mistake while changing what it wrote. The envelope's type is still enforced: a
// nested object is a different protocol pretending to be this one, so it is
// ErrServer rather than silently accepted.
//
// Usage is required to be countable and positive on every successful completion.
// Some OpenAI-compatible servers omit it, and the honest options were to refuse
// this response or to substitute EstimateTokens. Substituting would put a number
// the adapter made up into a field the budget settles against and cost accounting
// reads for every run, with nothing on the seam to say it was not measured — the
// silent-error class this project refuses. So an uncountable usage is ErrServer,
// naming the model and counts: loud, once, and attributable. A future
// "estimated usage" needs a field on the seam that says so, not a quiet fallback
// here.
func decodeOpenAIResponse(body []byte, model string) (Response, error) {
	var wire openAIWireResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return Response{}, fmt.Errorf("%w: model %q: the response body is not an OpenAI completion: %v", ErrServer, model, err)
	}
	if len(wire.Choices) == 0 {
		return Response{}, fmt.Errorf("%w: model %q: the response carried no choices", ErrServer, model)
	}
	choice := wire.Choices[0]
	resp := Response{}
	if choice.Message.Content != nil {
		resp.Content = *choice.Message.Content
	}
	if choice.FinishReason != nil {
		resp.Stop = *choice.FinishReason
	}
	for i, c := range choice.Message.ToolCalls {
		var arguments string
		if err := json.Unmarshal(c.Function.Arguments, &arguments); err != nil {
			return Response{}, fmt.Errorf("%w: model %q: tool call %d (%s) does not carry its arguments as a JSON string: %v",
				ErrServer, model, i, c.Function.Name, err)
		}
		resp.ToolCalls = append(resp.ToolCalls, Call{
			ID:        c.ID,
			Name:      c.Function.Name,
			Arguments: json.RawMessage(arguments),
		})
	}

	if wire.Usage == nil {
		return Response{}, fmt.Errorf("%w: model %q: the endpoint reported no usage; a completion the budget cannot settle against is not a success", ErrServer, model)
	}
	resp.Usage = Usage{In: wire.Usage.PromptTokens, Out: wire.Usage.CompletionTokens}
	if resp.Usage.In <= 0 || resp.Usage.Out <= 0 {
		return Response{}, fmt.Errorf("%w: model %q: usage %+v is not countable (prompt_tokens and completion_tokens must both be positive); the budget cannot settle against it",
			ErrServer, model, resp.Usage)
	}
	return resp, nil
}

// --- errors ---

// openAIErrorEnvelope is OpenAI's error body. Some servers in this family return
// a bare message or a plain-text body instead, so every caller of this type
// tolerates a decode failure and falls back to the body text.
type openAIErrorEnvelope struct {
	Error *struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
	} `json:"error"`
}

// openAIHTTPError classifies a non-2xx answer into the taxonomy, keeping the
// endpoint's own message so the operator sees what it said, and the model name so
// a multi-model endpoint's failure is attributable.
func openAIHTTPError(status int, body []byte, model string) error {
	class := classifyOpenAIStatus(status, body)
	where := fmt.Sprintf("provider: openai: HTTP %d", status)
	if model != "" {
		where = fmt.Sprintf("provider: openai: model %q: HTTP %d", model, status)
	}
	if detail := openAIErrorDetail(body); detail != "" {
		where += ": " + detail
	}
	return fmt.Errorf("%w: %s", class, where)
}

// classifyOpenAIStatus maps a status to one of the seven classes. The status is
// the primary signal because every server in this family uses the same codes for
// the same things; only a 400 is refined by its body, because context overflow is
// the one class this family reports as a 400 with a recognizable code or message.
//
// 403 is auth rather than bad_request: OpenAI uses it for a key that exists but
// cannot be used (a region, a disabled organization), and retrying it is exactly
// the "burn attempts on a bad credential" failure. A 3xx here means the client's
// redirect handling did not turn it into a completion, which is a server-side
// oddity rather than a request fault.
func classifyOpenAIStatus(status int, body []byte) error {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrAuth
	case status == http.StatusTooManyRequests:
		return ErrRateLimited
	case status == http.StatusNotFound:
		return ErrModelMissing
	case status == http.StatusRequestTimeout:
		return ErrTransport
	case status >= 500:
		return ErrServer
	case status >= 400:
		if isOpenAIContextOverflow(body) {
			return ErrContextOverflow
		}
		return ErrBadRequest
	default:
		return ErrServer
	}
}

// openAIContextOverflowPhrases are the spellings the family's servers use for
// "the prompt did not fit". They are matched case-insensitively against the whole
// body, so they work whether the phrase lives in message, type or code, and
// whether or not the body is the documented envelope. Only consulted for a 400;
// none of them appears in an ordinary malformed-request body.
var openAIContextOverflowPhrases = []string{
	"context_length_exceeded",
	"maximum context length",
	"context length",
	"context window",
	"too many tokens",
	"reduce the length",
	"prompt is too long",
	"input is too long",
	"exceeds the maximum number of tokens",
}

func isOpenAIContextOverflow(body []byte) bool {
	text := strings.ToLower(string(body))
	for _, phrase := range openAIContextOverflowPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// openAIErrorDetail is the endpoint's own message, with its code when it has one,
// truncated so a huge HTML error page cannot bury the class. A body that is not
// the documented envelope is used as-is: an operator can still read it, and
// discarding it would leave only "HTTP 400".
func openAIErrorDetail(body []byte) string {
	var envelope openAIErrorEnvelope
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error != nil && envelope.Error.Message != "" {
		detail := envelope.Error.Message
		var code string
		if err := json.Unmarshal(envelope.Error.Code, &code); err == nil && code != "" {
			detail += " (" + code + ")"
		}
		return truncateForError(detail)
	}
	return truncateForError(strings.TrimSpace(string(body)))
}

// truncateForError bounds an error message's size. It cuts at a rune boundary so
// the message stays valid UTF-8, and 400 bytes is enough for a real error message
// plus its code while keeping a multi-kilobyte HTML page from being echoed whole.
func truncateForError(s string) string {
	const limit = 400
	if len(s) <= limit {
		return s
	}
	n := limit
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

// isOpenAIModelsList reports whether a 2xx body looks like GET /v1/models. A
// probe that accepted any 200 would call a web page or a proxy's login page a
// healthy endpoint; the data field is what the route promises.
func isOpenAIModelsList(body []byte) bool {
	var list struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return false
	}
	return list.Data != nil || list.Object == "list"
}
