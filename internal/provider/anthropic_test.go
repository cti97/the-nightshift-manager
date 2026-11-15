package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake's wire types are deliberately not the adapter's. A fixture that
// decoded with the adapter's own structs would agree with it about every mistake
// the two share, so the fake decodes the Messages API shape as a stranger would
// and validates it field by field.
type fakeAnthropicRequest struct {
	Model      string                 `json:"model"`
	MaxTokens  *int                   `json:"max_tokens"`
	System     json.RawMessage        `json:"system"`
	Messages   []fakeAnthropicMessage `json:"messages"`
	Tools      []json.RawMessage      `json:"tools"`
	ToolChoice json.RawMessage        `json:"tool_choice"`
}

type fakeAnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type fakeAnthropicBlock struct {
	Type      string          `json:"type"`
	Text      *string         `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

type fakeAnthropicResponse struct {
	ID         string                 `json:"id"`
	Type       string                 `json:"type"`
	Role       string                 `json:"role"`
	Model      string                 `json:"model"`
	Content    []fakeAnthropicOut     `json:"content"`
	StopReason string                 `json:"stop_reason"`
	Usage      fakeAnthropicWireUsage `json:"usage"`
}

type fakeAnthropicOut struct {
	Type  string          `json:"type"`
	Text  *string         `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type fakeAnthropicWireUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// fakeAnthropicToolObject is a tools[] entry decoded in the test so the
// assertions read like the wire rather than like the adapter.
type fakeAnthropicToolObject struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicFakeRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// anthropicFake is the httptest endpoint Conformance is run against: one
// stateless handler that speaks the Messages API shape, validates the wire the
// way a real server does, and records what it received so the named adapter
// tests can inspect it.
type anthropicFake struct {
	URL string

	srv *httptest.Server
	mu  sync.Mutex
	// sent is every request the adapter issued, in order.
	sent []anthropicFakeRequest
}

func newAnthropicFake(t *testing.T) *anthropicFake {
	t.Helper()
	f := &anthropicFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	f.URL = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

func (f *anthropicFake) requests() []anthropicFakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]anthropicFakeRequest(nil), f.sent...)
}

func (f *anthropicFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *anthropicFake) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f.mu.Lock()
	f.sent = append(f.sent, anthropicFakeRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Header: r.Header.Clone(),
		Body:   body,
	})
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		io.WriteString(w, `{"data":[{"type":"model","id":"claude-conformance","display_name":"Claude (fake)","created_at":"2025-01-01T00:00:00Z"}],"has_more":false,"first_id":"claude-conformance","last_id":"claude-conformance"}`)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
		anthropicFailure(w, http.StatusNotFound, "not_found_error",
			fmt.Sprintf("the fake speaks GET /v1/models and POST /v1/messages, not %s %s", r.Method, r.URL.Path))
		return
	}

	var req fakeAnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		anthropicFailure(w, http.StatusBadRequest, "invalid_request_error", "the request body is not JSON: "+err.Error())
		return
	}
	if problem := fakeAnthropicWireProblem(req, r.Header); problem != "" {
		anthropicFailure(w, http.StatusBadRequest, "invalid_request_error", problem)
		return
	}

	// The four reserved conformance models answer with the family's real failure
	// bodies, so check 5 exercises the adapter's mapping rather than a made-up
	// status code.
	switch req.Model {
	case ConformanceModelAuth:
		anthropicFailure(w, http.StatusUnauthorized, "authentication_error", "invalid x-api-key")
		return
	case ConformanceModelRateLimited:
		anthropicFailure(w, http.StatusTooManyRequests, "rate_limit_error",
			"Number of request tokens has exceeded your per-minute rate limit (RPM): 40000")
		return
	case ConformanceModelContextOverflow:
		anthropicFailure(w, http.StatusBadRequest, "invalid_request_error",
			"prompt is too long: 300000 tokens > 200000 maximum")
		return
	case ConformanceModelServerError:
		anthropicFailure(w, http.StatusInternalServerError, "api_error", "Internal server error")
		return
	}

	// The usage is constant and positive: check 1 settles against it and check 3
	// requires two identical requests to report the same numbers.
	resp := fakeAnthropicResponse{
		ID:    "msg_fake",
		Type:  "message",
		Role:  "assistant",
		Model: req.Model,
		Usage: fakeAnthropicWireUsage{InputTokens: 37, OutputTokens: 12},
	}
	if len(req.Tools) > 0 && !fakeAnthropicHasToolResult(req) {
		text := "I will read the brief first."
		resp.Content = []fakeAnthropicOut{
			{Type: "text", Text: &text},
			{
				Type:  "tool_use",
				ID:    "toolu_fake_1",
				Name:  fakeAnthropicFirstToolName(req.Tools),
				Input: json.RawMessage(`{"path":"/tmp/brief.md","limit":10}`),
			},
		}
		resp.StopReason = "tool_use"
	} else {
		text := "The brief asks for a careful reading of the endpoint's health."
		resp.Content = []fakeAnthropicOut{{Type: "text", Text: &text}}
		resp.StopReason = "end_turn"
	}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, "the fake could not encode its response: "+err.Error(), http.StatusInternalServerError)
	}
}

// anthropicFailure writes the Messages API's own error envelope, so the
// adapter's classifier sees the documented shape rather than a made-up body.
func anthropicFailure(w http.ResponseWriter, status int, kind, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": kind, "message": message},
	})
}

// fakeAnthropicBlocks decodes a message's content as the protocol allows it: an
// array of content blocks (what this adapter always sends), or the string
// shorthand. A body that is neither is refused here rather than misread.
func fakeAnthropicBlocks(i int, raw json.RawMessage) ([]fakeAnthropicBlock, string) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Sprintf("messages[%d] has no content", i)
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, fmt.Sprintf("messages[%d].content = %s; want a JSON string or an array of blocks", i, raw)
		}
		return []fakeAnthropicBlock{{Type: "text", Text: &text}}, ""
	}
	if trimmed[0] != '[' {
		return nil, fmt.Sprintf("messages[%d].content = %s; want an array of content blocks or a string", i, raw)
	}
	var blocks []fakeAnthropicBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil, fmt.Sprintf("messages[%d].content is not an array of content blocks: %v", i, err)
	}
	return blocks, ""
}

// fakeAnthropicWireProblem is the fixture's half of the contract: it refuses a
// request that is not the Messages API shape. Every refusal here is a
// translation mistake the suite would otherwise only notice through a
// downstream symptom.
func fakeAnthropicWireProblem(req fakeAnthropicRequest, header http.Header) string {
	if header.Get("anthropic-version") == "" {
		return "the request carries no anthropic-version header, which every Messages API request requires"
	}
	if req.MaxTokens == nil {
		return "the request carries no max_tokens; the Messages API rejects a request without one"
	}
	if *req.MaxTokens <= 0 {
		return fmt.Sprintf("max_tokens = %d; want a positive reply cap", *req.MaxTokens)
	}
	if strings.TrimSpace(req.Model) == "" {
		return "the request names no model"
	}
	if len(req.System) > 0 {
		var system string
		if err := json.Unmarshal(req.System, &system); err != nil {
			return fmt.Sprintf("system = %s; want a JSON string (the top-level field, not a message)", req.System)
		}
	}

	for i, raw := range req.Tools {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return fmt.Sprintf("tools[%d] is not a JSON object: %v", i, err)
		}
		for key := range fields {
			if key != "name" && key != "description" && key != "input_schema" {
				return fmt.Sprintf("tools[%d] carries %q; a Messages API tool is exactly {name,description,input_schema}", i, key)
			}
		}
		if _, ok := fields["name"]; !ok {
			return fmt.Sprintf("tools[%d] has no name", i)
		}
		schema, ok := fields["input_schema"]
		if !ok {
			return fmt.Sprintf("tools[%d] has no input_schema; Anthropic's name for the tool's JSON Schema is input_schema, not parameters", i)
		}
		if trimmed := bytes.TrimSpace(schema); len(trimmed) == 0 || trimmed[0] != '{' {
			return fmt.Sprintf("tools[%d].input_schema = %s; want a JSON Schema object", i, schema)
		}
	}
	if len(req.Tools) > 0 && len(req.ToolChoice) == 0 {
		return "the request offers tools but carries no tool_choice; this fake requires the family's own field"
	}

	calls := make(map[string]bool)
	for i, m := range req.Messages {
		switch m.Role {
		case "user", "assistant":
		case "system":
			return fmt.Sprintf("messages[%d] has role %q; the system prompt is a top-level field and is never a message in this protocol", i, m.Role)
		case "tool":
			return fmt.Sprintf("messages[%d] has role %q; a tool result is a tool_result block in a user message, not a role", i, m.Role)
		default:
			return fmt.Sprintf("messages[%d] has role %q, which is not a Messages API role", i, m.Role)
		}
		blocks, problem := fakeAnthropicBlocks(i, m.Content)
		if problem != "" {
			return problem
		}
		if len(blocks) == 0 {
			return fmt.Sprintf("messages[%d] is an empty turn", i)
		}
		for j, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text == nil || *b.Text == "" {
					return fmt.Sprintf("messages[%d].content[%d] is a text block with no text", i, j)
				}
			case "tool_use":
				if m.Role != "assistant" {
					return fmt.Sprintf("messages[%d].content[%d] is a tool_use block in a %s turn; only an assistant turn carries the model's calls", i, j, m.Role)
				}
				if b.ID == "" {
					return fmt.Sprintf("messages[%d].content[%d] is a tool_use block with no id", i, j)
				}
				if b.Name == "" {
					return fmt.Sprintf("messages[%d].content[%d] is a tool_use block with no name", i, j)
				}
				if trimmed := bytes.TrimSpace(b.Input); len(trimmed) == 0 || trimmed[0] != '{' {
					return fmt.Sprintf("messages[%d].content[%d].input = %s; want a JSON object", i, j, b.Input)
				}
				calls[b.ID] = true
			case "tool_result":
				if m.Role != "user" {
					return fmt.Sprintf("messages[%d].content[%d] is a tool_result block in an assistant turn; the protocol sends results in a user message", i, j)
				}
				if b.ToolUseID == "" {
					return fmt.Sprintf("messages[%d].content[%d] is a tool_result block with no tool_use_id", i, j)
				}
				if !calls[b.ToolUseID] {
					return fmt.Sprintf("messages[%d].content[%d] answers tool_use_id %q, which no preceding assistant turn carries", i, j, b.ToolUseID)
				}
				if len(bytes.TrimSpace(b.Content)) == 0 {
					return fmt.Sprintf("messages[%d].content[%d] is a tool_result block with no content", i, j)
				}
			default:
				return fmt.Sprintf("messages[%d].content[%d] has type %q, which is not a content block this project sends", i, j, b.Type)
			}
		}
	}
	return ""
}

// fakeAnthropicHasToolResult reports whether the request already carries a tool
// result, which is how the fake decides between the tool_use turn and the final
// answer. Validation has already refused a malformed one.
func fakeAnthropicHasToolResult(req fakeAnthropicRequest) bool {
	for i, m := range req.Messages {
		blocks, problem := fakeAnthropicBlocks(i, m.Content)
		if problem != "" {
			continue
		}
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return true
			}
		}
	}
	return false
}

// fakeAnthropicFirstToolName reads the first offered tool's name. The validation
// above has already refused a request whose tools have no name, so an empty
// result here would be a hole in the fixture rather than a case to report.
func fakeAnthropicFirstToolName(tools []json.RawMessage) string {
	for _, raw := range tools {
		var tool struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &tool); err == nil && tool.Name != "" {
			return tool.Name
		}
	}
	return ""
}

func newAnthropicTestProvider(t *testing.T, cfg Config) *Anthropic {
	t.Helper()
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	p, err := NewAnthropic(cfg)
	if err != nil {
		t.Fatalf("NewAnthropic(%+v): %v", cfg, err)
	}
	return p
}

// anthropicErrorText is the message a Messages API error body carries, or "" for
// a body that is not that shape.
func anthropicErrorText(body string) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		return ""
	}
	return envelope.Error.Message
}

// TestAnthropicPassesConformance is the suite's verdict on the adapter, against
// a fake endpoint that speaks the real Messages API shape. The provider is built
// directly and with no endpoint reach, which is what check 6 expects.
func TestAnthropicPassesConformance(t *testing.T) {
	fake := newAnthropicFake(t)
	Conformance(t, func() Provider {
		p, err := NewAnthropic(Config{BaseURL: fake.URL, APIKey: "test-key", Timeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("NewAnthropic: %v", err)
		}
		return p
	})
}

// TestAnthropicKeepsSystemOutOfTheMessages is the first brief-named test, and it
// asserts the *absence* the brief calls the most common bug in this adapter: the
// system prompt is a top-level field, never an entry in messages. A test that only
// checked for a top-level "system" would pass an adapter that sent it twice —
// once top-level and once as a role:"system" message — so the absence legs are
// the point:
//
//   - the serialized body contains the literal `"role":"system"` nowhere;
//   - every messages[] role is user or assistant;
//   - the system prompt did not become a turn (messages still holds one user
//     turn, not two).
func TestAnthropicKeepsSystemOutOfTheMessages(t *testing.T) {
	ctx := context.Background()
	fake := newAnthropicFake(t)
	p := newAnthropicTestProvider(t, Config{BaseURL: fake.URL, APIKey: "test-key"})
	req := Request{
		Model:     "claude-sonnet-4-5",
		System:    "You are a careful night-shift worker.",
		MaxTokens: 128,
		Messages:  []Message{{Role: "user", Content: "Read the brief and say what it asks for."}},
	}
	if _, err := p.Complete(ctx, req); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	sent := fake.requests()
	if len(sent) != 1 {
		t.Fatalf("the adapter sent %d requests, want 1", len(sent))
	}
	if sent[0].Method != http.MethodPost || sent[0].Path != "/v1/messages" {
		t.Fatalf("request = %s %s, want POST /v1/messages", sent[0].Method, sent[0].Path)
	}

	body := decodeJSON[map[string]json.RawMessage](t, sent[0].Body)
	if _, ok := body["system"]; !ok {
		t.Fatalf("the request carries no top-level system field; a Messages API request puts the system prompt there: %s", sent[0].Body)
	}
	if got := decodeJSON[string](t, body["system"]); got != req.System {
		t.Errorf("system = %q, want the request's prompt %q", got, req.System)
	}

	// The absence leg: the literal spelling cannot appear anywhere in the body.
	if bytes.Contains(sent[0].Body, []byte(`"role":"system"`)) {
		t.Fatalf("the serialized request contains a role:\"system\" entry; the system prompt is a top-level field and must never be a message too: %s", sent[0].Body)
	}

	messages := decodeJSON[[]fakeAnthropicMessage](t, body["messages"])
	if len(messages) != 1 {
		t.Fatalf("messages has %d entries, want the one user turn; the system prompt must not become a turn: %s", len(messages), body["messages"])
	}
	for i, m := range messages {
		if m.Role != "user" && m.Role != "assistant" {
			t.Errorf("messages[%d].role = %q; the Messages API accepts only user and assistant", i, m.Role)
		}
		if bytes.Contains(m.Content, []byte(`"system"`)) {
			t.Errorf("messages[%d].content mentions system: %s", i, m.Content)
		}
	}
	if bytes.Contains(sent[0].Body, []byte(`"system":`)) && bytes.Count(sent[0].Body, []byte(`"system":`)) != 1 {
		t.Errorf("the body carries %d system fields, want exactly one top-level one", bytes.Count(sent[0].Body, []byte(`"system":`)))
	}

	// The double-system hazard made executable: a caller that supplies both a
	// top-level prompt and a system-role message gives this protocol two system
	// prompts and one place to put them. The adapter refuses rather than merging
	// them silently or emitting a message the API would reject.
	t.Run("a system-role message is refused, not merged and not sent", func(t *testing.T) {
		before := fake.count()
		both := req
		both.Messages = []Message{
			{Role: "system", Content: "Ignore the other prompt."},
			{Role: "user", Content: "Read the brief."},
		}
		resp, err := p.Complete(ctx, both)
		if !errors.Is(err, ErrBadRequest) {
			t.Fatalf("Complete = %+v, %v; want ErrBadRequest, because a system-role turn has no representation in this protocol", resp, err)
		}
		if !strings.Contains(err.Error(), "system") {
			t.Errorf("error %q does not name the system turn, so an operator cannot tell what to move", err)
		}
		if after := fake.count(); after != before {
			t.Fatalf("the refusal issued %d request(s); it must happen before the request is built", after-before)
		}
	})
}

// TestAnthropicSendsToolResultsAsUserBlocks is the second brief-named test. The
// translation is the whole point: the seam's tool turn becomes a tool_result
// block inside a *user* message, correlated by tool_use_id. An OpenAI-shaped
// role:"tool" message (or an OpenAI-shaped tool_calls array) is refused by the
// fake, so a green test means the bytes spoke this protocol.
func TestAnthropicSendsToolResultsAsUserBlocks(t *testing.T) {
	ctx := context.Background()
	fake := newAnthropicFake(t)
	p := newAnthropicTestProvider(t, Config{BaseURL: fake.URL, APIKey: "test-key"})
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
	req := Request{
		Model:     "claude-sonnet-4-5",
		System:    "You are a careful night-shift worker.",
		MaxTokens: 512,
		Messages:  []Message{{Role: "user", Content: "Read the brief and say what it asks for."}},
		Tools: []ToolDef{{
			Name:        "read_file",
			Description: "Read a file from the run directory and return its text.",
			Schema:      schema,
		}},
	}

	first, err := p.Complete(ctx, req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(first.ToolCalls) != 1 {
		t.Fatalf("Complete returned %d tool calls, want the fake's one call (response %+v)", len(first.ToolCalls), first)
	}
	call := first.ToolCalls[0]
	const toolResult = `{"content":"# The brief"}`

	// The conversation returns the way the seam defines it: the assistant turn
	// replays the call it made, and the result names it in Message.CallID.
	req.Messages = append(req.Messages,
		Message{Role: "assistant", Content: first.Content, ToolCalls: first.ToolCalls},
		Message{Role: "tool", Name: call.Name, CallID: call.ID, Content: toolResult},
	)
	if _, err := p.Complete(ctx, req); err != nil {
		t.Fatalf("Complete with a tool result: %v", err)
	}

	sent := fake.requests()
	if len(sent) != 2 {
		t.Fatalf("the adapter sent %d requests, want 2", len(sent))
	}
	body := decodeJSON[map[string]json.RawMessage](t, sent[1].Body)
	messages := decodeJSON[[]fakeAnthropicMessage](t, body["messages"])
	if len(messages) != 3 {
		t.Fatalf("the replayed conversation has %d messages, want user, assistant, user: %s", len(messages), body["messages"])
	}

	for i, m := range messages {
		if m.Role == "tool" {
			t.Fatalf("messages[%d].role = \"tool\"; this protocol has no tool role, and the result belongs in a tool_result block in a user message", i)
		}
	}
	if got := messages[2].Role; got != "user" {
		t.Fatalf("messages[2].role = %q, want user: a tool result is a tool_result block in a user message", got)
	}

	// The assistant turn replays the model's calls as tool_use blocks.
	assistantBlocks := decodeJSON[[]fakeAnthropicBlock](t, messages[1].Content)
	var toolUse *fakeAnthropicBlock
	for i, b := range assistantBlocks {
		if b.Type == "tool_use" {
			toolUse = &assistantBlocks[i]
		}
	}
	if toolUse == nil {
		t.Fatalf("the assistant turn carries no tool_use block: %s", messages[1].Content)
	}
	if toolUse.ID != call.ID || toolUse.Name != call.Name {
		t.Errorf("replayed tool_use = %+v, want id %q and name %q", toolUse, call.ID, call.Name)
	}
	if !bytes.Equal(toolUse.Input, call.Arguments) {
		t.Errorf("replayed input = %s, want the call's own bytes %s", toolUse.Input, call.Arguments)
	}

	// The result: one tool_result block, naming the call, carrying the text.
	resultBlocks := decodeJSON[[]fakeAnthropicBlock](t, messages[2].Content)
	if len(resultBlocks) != 1 {
		t.Fatalf("the user turn carries %d blocks, want the one tool_result: %s", len(resultBlocks), messages[2].Content)
	}
	result := resultBlocks[0]
	if result.Type != "tool_result" {
		t.Fatalf("the result block is type %q, want tool_result", result.Type)
	}
	if result.ToolUseID != call.ID {
		t.Errorf("tool_result.tool_use_id = %q, want the call's id %q, or the result cannot be correlated with it", result.ToolUseID, call.ID)
	}
	if got := decodeJSON[string](t, result.Content); got != toolResult {
		t.Errorf("tool_result.content = %q, want the tool result %q", got, toolResult)
	}
	// A tool_result block is identified by tool_use_id; there is no name field on
	// the wire, and sending one would be the OpenAI-shaped habit this test exists
	// to catch.
	if bytes.Contains(messages[2].Content, []byte(`"name"`)) {
		t.Errorf("the tool_result block carries a name: %s", messages[2].Content)
	}
	// Nothing OpenAI-shaped may survive the translation.
	for _, banned := range []string{`"tool_calls"`, `"tool_call_id"`, `"function"`, `"parameters"`} {
		if bytes.Contains(sent[1].Body, []byte(banned)) {
			t.Errorf("the serialized request carries %s, which is the OpenAI shape, not this protocol's", banned)
		}
	}

	// Two results for two calls are one user turn with two blocks: the protocol
	// requires every result for one assistant turn to arrive together, and
	// splitting them into two consecutive user messages is a 400.
	t.Run("parallel results arrive in one user message", func(t *testing.T) {
		parallel := newAnthropicFake(t)
		q := newAnthropicTestProvider(t, Config{BaseURL: parallel.URL})
		callA := Call{ID: "toolu_a", Name: "read_file", Arguments: json.RawMessage(`{"path":"/tmp/a.md"}`)}
		callB := Call{ID: "toolu_b", Name: "read_file", Arguments: json.RawMessage(`{"path":"/tmp/b.md"}`)}
		_, err := q.Complete(ctx, Request{
			Model:     "claude-sonnet-4-5",
			MaxTokens: 256,
			Messages: []Message{
				{Role: "user", Content: "Read both files."},
				{Role: "assistant", ToolCalls: []Call{callA, callB}},
				{Role: "tool", Name: callA.Name, CallID: callA.ID, Content: "a"},
				{Role: "tool", Name: callB.Name, CallID: callB.ID, Content: "b"},
			},
		})
		if err != nil {
			t.Fatalf("Complete with two tool results: %v", err)
		}
		reqs := parallel.requests()
		if len(reqs) != 1 {
			t.Fatalf("the adapter sent %d requests, want 1", len(reqs))
		}
		wire := decodeJSON[map[string]json.RawMessage](t, reqs[0].Body)
		got := decodeJSON[[]fakeAnthropicMessage](t, wire["messages"])
		if len(got) != 3 {
			t.Fatalf("the request has %d messages, want user, assistant, user: %s", len(got), wire["messages"])
		}
		if got[2].Role != "user" {
			t.Fatalf("the last message is %q, want user", got[2].Role)
		}
		blocks := decodeJSON[[]fakeAnthropicBlock](t, got[2].Content)
		if len(blocks) != 2 {
			t.Fatalf("the user turn carries %d blocks, want one tool_result per call: %s", len(blocks), got[2].Content)
		}
		for i, want := range []string{callA.ID, callB.ID} {
			if blocks[i].Type != "tool_result" || blocks[i].ToolUseID != want {
				t.Errorf("blocks[%d] = %+v, want a tool_result for %q", i, blocks[i], want)
			}
		}
	})
}

// TestAnthropicDecodesToolUseBlocks is the third brief-named test: a tool_use
// content block becomes a Call with its id and input preserved. The input is
// deliberately not in map order, because a decode-into-any-then-marshal round
// trip sorts the keys and would break a call that names its arguments in the
// model's order.
func TestAnthropicDecodesToolUseBlocks(t *testing.T) {
	// The escaped quote is there because the value travels inside a JSON string.
	const arguments = `{"path":"/tmp/a\"b.md","limit":10,"z":true,"a":null}`
	const body = `{"id":"msg_v","type":"message","role":"assistant","model":"claude-sonnet-4-5",` +
		`"content":[{"type":"text","text":"Reading the brief."},` +
		`{"type":"tool_use","id":"toolu_v","name":"read_file","input":` + arguments + `}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":11,"output_tokens":7}}`

	srv := newStatusServer(t, http.StatusOK, body)
	p := newAnthropicTestProvider(t, Config{BaseURL: srv.URL})
	resp, err := p.Complete(context.Background(), Request{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 64,
		Messages:  []Message{{Role: "user", Content: "Read the brief."}},
		Tools:     []ToolDef{{Name: "read_file", Schema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "Reading the brief." {
		t.Errorf("Content = %q, want the text block's text", resp.Content)
	}
	if resp.Stop != "tool_use" {
		t.Errorf("Stop = %q, want the stop_reason", resp.Stop)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("Complete returned %d tool calls, want 1", len(resp.ToolCalls))
	}
	got := resp.ToolCalls[0]
	if got.ID != "toolu_v" || got.Name != "read_file" {
		t.Errorf("call = %+v, want id toolu_v and name read_file", got)
	}
	if string(got.Arguments) != arguments {
		t.Fatalf("arguments = %s, want %s byte-for-byte; a decode-into-any-then-marshal round trip reorders keys and breaks the tool call", got.Arguments, arguments)
	}

	// Proof that the fixture discriminates: if a map round trip reproduced the
	// bytes, the assertion above would pass for the adapter this test exists to
	// reject.
	var naive any
	if err := json.Unmarshal([]byte(arguments), &naive); err != nil {
		t.Fatalf("the fixture's arguments are not valid JSON: %v", err)
	}
	roundTripped, err := json.Marshal(naive)
	if err != nil {
		t.Fatalf("marshalling the fixture's arguments: %v", err)
	}
	if string(roundTripped) == arguments {
		t.Fatal("the fixture's arguments survive a map round trip unchanged, so this test cannot detect key reordering")
	}

	// A tool_use block the seam could never correlate is not a call: it is a
	// response the adapter must refuse rather than return with an empty id.
	t.Run("a tool_use with no id is refused", func(t *testing.T) {
		srv := newStatusServer(t, http.StatusOK, `{"id":"msg_x","type":"message","role":"assistant","content":[{"type":"tool_use","name":"read_file","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":11,"output_tokens":7}}`)
		p := newAnthropicTestProvider(t, Config{BaseURL: srv.URL})
		_, err := p.Complete(context.Background(), Request{
			Model:     "claude-sonnet-4-5",
			MaxTokens: 64,
			Messages:  []Message{{Role: "user", Content: "Read the brief."}},
		})
		if !errors.Is(err, ErrServer) {
			t.Fatalf("Complete = %v, want ErrServer for a tool_use block with no id", err)
		}
	})

	t.Run("a tool_use with no name is refused", func(t *testing.T) {
		srv := newStatusServer(t, http.StatusOK, `{"id":"msg_x","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_x","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":11,"output_tokens":7}}`)
		p := newAnthropicTestProvider(t, Config{BaseURL: srv.URL})
		_, err := p.Complete(context.Background(), Request{
			Model:     "claude-sonnet-4-5",
			MaxTokens: 64,
			Messages:  []Message{{Role: "user", Content: "Read the brief."}},
		})
		if !errors.Is(err, ErrServer) {
			t.Fatalf("Complete = %v, want ErrServer for a tool_use block with no name", err)
		}
	})

	// A block type this seam does not model is not a broken response: a
	// thinking block carries no content and no call, so it is skipped rather
	// than turned into an error that would make the adapter unusable against
	// any model that thinks out loud.
	t.Run("an unmodelled block type is skipped", func(t *testing.T) {
		srv := newStatusServer(t, http.StatusOK, `{"id":"msg_x","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"The brief is planned."}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7}}`)
		p := newAnthropicTestProvider(t, Config{BaseURL: srv.URL})
		resp, err := p.Complete(context.Background(), Request{
			Model:     "claude-sonnet-4-5",
			MaxTokens: 64,
			Messages:  []Message{{Role: "user", Content: "Read the brief."}},
		})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if resp.Content != "The brief is planned." {
			t.Errorf("Content = %q, want the text block's text", resp.Content)
		}
	})
}

// TestAnthropicSendsToolsWithInputSchema pins the tool translation: the seam's
// ToolDef carries name/description/schema, and this family puts the schema in
// input_schema on the tool itself — not inside a function object, and not under
// OpenAI's parameters.
func TestAnthropicSendsToolsWithInputSchema(t *testing.T) {
	fake := newAnthropicFake(t)
	p := newAnthropicTestProvider(t, Config{BaseURL: fake.URL})
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
	if _, err := p.Complete(context.Background(), Request{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 128,
		Messages:  []Message{{Role: "user", Content: "Read the brief."}},
		Tools:     []ToolDef{{Name: "read_file", Description: "Read a file and return its text.", Schema: schema}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	sent := fake.requests()
	if len(sent) != 1 {
		t.Fatalf("the adapter sent %d requests, want 1", len(sent))
	}
	body := decodeJSON[map[string]json.RawMessage](t, sent[0].Body)
	tools := decodeJSON[[]map[string]json.RawMessage](t, body["tools"])
	if len(tools) != 1 {
		t.Fatalf("tools has %d entries, want 1", len(tools))
	}
	if len(tools[0]) != 3 {
		t.Errorf("tools[0] has keys %v; a Messages API tool is exactly {name,description,input_schema}", tools[0])
	}
	if _, ok := tools[0]["parameters"]; ok {
		t.Error("tools[0] carries \"parameters\"; this family calls the tool's schema input_schema")
	}
	if _, ok := tools[0]["function"]; ok {
		t.Error("tools[0] carries a \"function\" object; this family puts name and description on the tool itself")
	}
	tool := decodeJSON[fakeAnthropicToolObject](t, mustMarshal(t, tools[0]))
	if tool.Name != "read_file" || tool.Description != "Read a file and return its text." {
		t.Errorf("tool = %+v, want the request's name and description", tool)
	}
	if !jsonEqual(t, tool.InputSchema, schema) {
		t.Errorf("input_schema = %s, want the request's schema %s", tool.InputSchema, schema)
	}
	// tool_choice is an object here, not OpenAI's string; the type field is what
	// says the model may choose.
	var choice struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body["tool_choice"], &choice); err != nil || choice.Type != "auto" {
		t.Errorf("tool_choice = %s, want {\"type\":\"auto\"}", body["tool_choice"])
	}
}

// mustMarshal re-encodes a decoded object so a struct can be decoded from it.
func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling %+v: %v", v, err)
	}
	return raw
}

// TestAnthropicRefusesARequestWithNoReplyCap pins the max_tokens decision. The
// Messages API requires max_tokens on every request, and the seam's MaxTokens is
// where the caller decides how much a step may spend on output. The adapter does
// not invent a cap:
//
//   - a default would make the adapter, not the caller, decide the cost of every
//     step, and the budget's ceiling is a safety mechanism, not a suggestion;
//   - a small default truncates the reply silently (stop_reason "max_tokens"),
//     possibly mid-tool-call, which is the silent-error class this project
//     refuses; a large one silently spends what the caller did not authorise;
//   - the conformance suite supplies MaxTokens itself precisely so an adapter
//     does not have to invent one.
//
// So a request that sets no cap is refused locally, before anything is built,
// with the protocol's requirement in the message.
func TestAnthropicRefusesARequestWithNoReplyCap(t *testing.T) {
	ctx := context.Background()
	fake := newAnthropicFake(t)
	p := newAnthropicTestProvider(t, Config{BaseURL: fake.URL})

	for _, tc := range []struct {
		name string
		cap  int
	}{
		{"no cap at all", 0},
		{"a negative cap", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := fake.count()
			resp, err := p.Complete(ctx, Request{
				Model:     "claude-sonnet-4-5",
				MaxTokens: tc.cap,
				Messages:  []Message{{Role: "user", Content: "Read the brief."}},
			})
			if !errors.Is(err, ErrBadRequest) {
				t.Fatalf("Complete = %+v, %v; want ErrBadRequest for max_tokens %d", resp, err, tc.cap)
			}
			if !strings.Contains(err.Error(), "max_tokens") {
				t.Errorf("error %q does not name max_tokens, so an operator cannot tell what to set", err)
			}
			if after := fake.count(); after != before {
				t.Fatalf("the refusal issued %d request(s); it must happen before the request is built", after-before)
			}
		})
	}

	// The control: a cap the caller set goes out unchanged, which is what makes
	// the refusals above about a missing decision rather than a missing feature.
	if _, err := p.Complete(ctx, Request{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 256,
		Messages:  []Message{{Role: "user", Content: "Read the brief."}},
	}); err != nil {
		t.Fatalf("Complete with a cap: %v", err)
	}
	sent := fake.requests()
	if len(sent) != 1 {
		t.Fatalf("the control sent %d requests, want 1", len(sent))
	}
	body := decodeJSON[map[string]json.RawMessage](t, sent[0].Body)
	if got := decodeJSON[int](t, body["max_tokens"]); got != 256 {
		t.Errorf("max_tokens = %d, want the caller's 256", got)
	}
}

// TestAnthropicSetsTheRequiredHeaders pins the two headers this family requires
// on every request, on both routes.
func TestAnthropicSetsTheRequiredHeaders(t *testing.T) {
	ctx := context.Background()
	fake := newAnthropicFake(t)
	p := newAnthropicTestProvider(t, Config{BaseURL: fake.URL, APIKey: "sk-ant-test"})
	if _, err := p.Complete(ctx, Request{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 16,
		Messages:  []Message{{Role: "user", Content: "Read the brief."}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, err := p.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	sent := fake.requests()
	if len(sent) != 2 {
		t.Fatalf("the adapter sent %d requests, want a completion and a probe", len(sent))
	}
	for i, r := range sent {
		if got := r.Header.Get("anthropic-version"); got != anthropicAPIVersion {
			t.Errorf("request %d: anthropic-version = %q, want %q", i, got, anthropicAPIVersion)
		}
		if got := r.Header.Get("x-api-key"); got != "sk-ant-test" {
			t.Errorf("request %d: x-api-key = %q, want the configured key", i, got)
		}
	}
	if got := sent[0].Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	// No key configured is not an empty x-api-key header, which the API reads as
	// a credential and rejects.
	keyless := newAnthropicFake(t)
	q := newAnthropicTestProvider(t, Config{BaseURL: keyless.URL})
	if _, err := q.Complete(ctx, Request{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 16,
		Messages:  []Message{{Role: "user", Content: "Read the brief."}},
	}); err != nil {
		t.Fatalf("Complete without a key: %v", err)
	}
	if got := keyless.requests()[0].Header.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key = %q with no key configured, want no header", got)
	}
}

// TestAnthropicMapsAnErrorBodyToATaxonomySentinel covers the taxonomy. The status
// is the primary signal; only a 400's body refines it into context overflow,
// because that is how this family reports "the prompt did not fit". The 429 case
// is the brief's named one and is first in the table.
func TestAnthropicMapsAnErrorBodyToATaxonomySentinel(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{
			name:   "429 a real rate-limit body",
			status: http.StatusTooManyRequests,
			body:   `{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit (RPM): 40000"}}`,
			want:   ErrRateLimited,
		},
		{
			name:   "429 a body that is not JSON",
			status: http.StatusTooManyRequests,
			body:   "too many requests",
			want:   ErrRateLimited,
		},
		{
			name:   "401 an invalid key",
			status: http.StatusUnauthorized,
			body:   `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			want:   ErrAuth,
		},
		{
			name:   "403 a key without access to the model",
			status: http.StatusForbidden,
			body:   `{"type":"error","error":{"type":"permission_error","message":"Your API key does not have permission to use the specified resource."}}`,
			want:   ErrAuth,
		},
		{
			name:   "404 a model the endpoint does not have",
			status: http.StatusNotFound,
			body:   `{"type":"error","error":{"type":"not_found_error","message":"model: claude-9"}}`,
			want:   ErrModelMissing,
		},
		{
			name:   "400 a prompt that is too long",
			status: http.StatusBadRequest,
			body:   `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 300000 tokens > 200000 maximum"}}`,
			want:   ErrContextOverflow,
		},
		{
			name:   "400 a context overflow spelled another way",
			status: http.StatusBadRequest,
			body:   `{"type":"error","error":{"type":"invalid_request_error","message":"input length and max_tokens exceed context limit: 210000 + 8192 > 200000"}}`,
			want:   ErrContextOverflow,
		},
		{
			name:   "413 a request too large to fit",
			status: http.StatusRequestEntityTooLarge,
			body:   `{"type":"error","error":{"type":"request_too_large","message":"Request exceeds the maximum allowed number of bytes."}}`,
			want:   ErrContextOverflow,
		},
		{
			name:   "400 a malformed request of another kind",
			status: http.StatusBadRequest,
			body:   `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: Input should be greater than or equal to 1"}}`,
			want:   ErrBadRequest,
		},
		{
			name:   "500 a server error",
			status: http.StatusInternalServerError,
			body:   `{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`,
			want:   ErrServer,
		},
		{
			name:   "529 the endpoint is overloaded",
			status: 529,
			body:   `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			want:   ErrServer,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newStatusServer(t, tc.status, tc.body)
			p := newAnthropicTestProvider(t, Config{BaseURL: srv.URL})
			resp, err := p.Complete(context.Background(), Request{
				Model:     "claude-sonnet-4-5",
				MaxTokens: 16,
				Messages:  []Message{{Role: "user", Content: "Read the brief."}},
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Complete = %+v, %v; want an error satisfying errors.Is(err, %v)", resp, err, tc.want)
			}
			if !zeroResponse(resp) {
				t.Errorf("a failed Complete returned a response: %+v", resp)
			}
			if status := "HTTP " + strconv.Itoa(tc.status); !strings.Contains(err.Error(), status) {
				t.Errorf("error %q does not name %q, so an operator cannot tell what the endpoint said", err, status)
			}
			if body := anthropicErrorText(tc.body); body != "" && !strings.Contains(err.Error(), body) {
				t.Errorf("error %q does not carry the endpoint's message %q", err, body)
			}
		})
	}

	t.Run("a transport failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		p := newAnthropicTestProvider(t, Config{BaseURL: url})
		if _, err := p.Complete(context.Background(), Request{
			Model:     "claude-sonnet-4-5",
			MaxTokens: 16,
			Messages:  []Message{{Role: "user", Content: "Read the brief."}},
		}); !errors.Is(err, ErrTransport) {
			t.Fatalf("Complete against a closed server = %v, want ErrTransport", err)
		}
	})
}

// TestAnthropicRequiresAUsageTheBudgetCanSettle: the seam's Usage has no
// "estimated" flag, so a provider that substituted its own estimate for an
// absent one would be indistinguishable from a measuring one. An endpoint that
// answers 200 without a countable usage is refused rather than settled against a
// number the adapter made up.
func TestAnthropicRequiresAUsageTheBudgetCanSettle(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{
			name: "no usage at all",
			body: `{"id":"msg_x","type":"message","role":"assistant","content":[{"type":"text","text":"The brief is planned."}],"stop_reason":"end_turn"}`,
			want: ErrServer,
		},
		{
			name: "a zero input count",
			body: `{"id":"msg_x","type":"message","role":"assistant","content":[{"type":"text","text":"The brief is planned."}],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":12}}`,
			want: ErrServer,
		},
		{
			name: "a zero output count",
			body: `{"id":"msg_x","type":"message","role":"assistant","content":[{"type":"text","text":"The brief is planned."}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":0}}`,
			want: ErrServer,
		},
		{
			name: "no content blocks",
			body: `{"id":"msg_x","type":"message","role":"assistant","content":[],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7}}`,
			want: ErrServer,
		},
		{
			name: "a body that is not a message",
			body: `not json`,
			want: ErrServer,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newStatusServer(t, http.StatusOK, tc.body)
			p := newAnthropicTestProvider(t, Config{BaseURL: srv.URL})
			resp, err := p.Complete(context.Background(), Request{
				Model:     "claude-sonnet-4-5",
				MaxTokens: 16,
				Messages:  []Message{{Role: "user", Content: "Read the brief."}},
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Complete = %+v, %v; want an error satisfying errors.Is(err, %v)", resp, err, tc.want)
			}
			if !zeroResponse(resp) {
				t.Errorf("a failed Complete returned a response: %+v", resp)
			}
		})
	}

	// The control: a countable usage is accepted and reported as measured.
	srv := newStatusServer(t, http.StatusOK, `{"id":"msg_x","type":"message","role":"assistant","content":[{"type":"text","text":"The brief is planned."}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":7}}`)
	p := newAnthropicTestProvider(t, Config{BaseURL: srv.URL})
	resp, err := p.Complete(context.Background(), Request{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 16,
		Messages:  []Message{{Role: "user", Content: "Read the brief."}},
	})
	if err != nil {
		t.Fatalf("Complete with a countable usage: %v", err)
	}
	if resp.Usage != (Usage{In: 11, Out: 7}) {
		t.Errorf("Usage = %+v, want the endpoint's input/output counts", resp.Usage)
	}
	if resp.Content != "The brief is planned." || resp.Stop != "end_turn" {
		t.Errorf("response = %+v, want the endpoint's text and stop reason", resp)
	}
}

// TestAnthropicProbeAsksForTheModelList is the probe's cost, stated as a test:
// one GET /v1/models — authenticated, no tokens billed and no model loaded,
// because this family has no free route that says more and a completion canary
// would spend money on every probe. The states are the answer to "what should an
// operator do".
func TestAnthropicProbeAsksForTheModelList(t *testing.T) {
	t.Run("an endpoint that lists models", func(t *testing.T) {
		fake := newAnthropicFake(t)
		p := newAnthropicTestProvider(t, Config{BaseURL: fake.URL, APIKey: "test-key"})
		health, err := p.Probe(context.Background())
		if err != nil || health != HealthOK {
			t.Fatalf("Probe = %q, %v; want ok, nil", health, err)
		}
		sent := fake.requests()
		if len(sent) != 1 {
			t.Fatalf("Probe sent %d requests, want 1", len(sent))
		}
		if sent[0].Method != http.MethodGet || sent[0].Path != "/v1/models" {
			t.Errorf("Probe request = %s %s, want GET /v1/models", sent[0].Method, sent[0].Path)
		}
	})

	t.Run("a key the endpoint rejects", func(t *testing.T) {
		srv := newStatusServer(t, http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
		p := newAnthropicTestProvider(t, Config{BaseURL: srv.URL})
		if health, err := p.Probe(context.Background()); err != nil || health != HealthAuthFailed {
			t.Fatalf("Probe = %q, %v; want auth_failed, nil", health, err)
		}
	})

	t.Run("a server that answers 500", func(t *testing.T) {
		srv := newStatusServer(t, http.StatusInternalServerError, `{"type":"error","error":{"type":"api_error","message":"Internal server error"}}`)
		p := newAnthropicTestProvider(t, Config{BaseURL: srv.URL})
		if health, err := p.Probe(context.Background()); err != nil || health != HealthDown {
			t.Fatalf("Probe = %q, %v; want down, nil", health, err)
		}
	})

	t.Run("an overloaded endpoint", func(t *testing.T) {
		srv := newStatusServer(t, 529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
		p := newAnthropicTestProvider(t, Config{BaseURL: srv.URL})
		if health, err := p.Probe(context.Background()); err != nil || health != HealthDown {
			t.Fatalf("Probe = %q, %v; want down, nil", health, err)
		}
	})

	t.Run("a route that does not exist", func(t *testing.T) {
		srv := newStatusServer(t, http.StatusNotFound, `{"type":"error","error":{"type":"not_found_error","message":"Not Found"}}`)
		p := newAnthropicTestProvider(t, Config{BaseURL: srv.URL})
		health, err := p.Probe(context.Background())
		if !errors.Is(err, ErrModelMissing) {
			t.Fatalf("Probe = %q, %v; want ErrModelMissing, because the endpoint answered and the route is wrong", health, err)
		}
		if health != "" {
			t.Errorf("Probe reported health %q alongside an error; a non-nil error means no state was determined", health)
		}
	})

	t.Run("an endpoint that is throttling", func(t *testing.T) {
		srv := newStatusServer(t, http.StatusTooManyRequests, `{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited"}}`)
		p := newAnthropicTestProvider(t, Config{BaseURL: srv.URL})
		if health, err := p.Probe(context.Background()); !errors.Is(err, ErrRateLimited) || health != "" {
			t.Fatalf("Probe = %q, %v; want ErrRateLimited and no health state", health, err)
		}
	})

	t.Run("a 200 that is not a model list", func(t *testing.T) {
		srv := newStatusServer(t, http.StatusOK, "<html><body>a web page, not an API</body></html>")
		p := newAnthropicTestProvider(t, Config{BaseURL: srv.URL})
		if health, err := p.Probe(context.Background()); !errors.Is(err, ErrServer) || health != "" {
			t.Fatalf("Probe = %q, %v; want ErrServer for a 200 that is not a model list", health, err)
		}
	})

	t.Run("an unreachable endpoint", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		p := newAnthropicTestProvider(t, Config{BaseURL: url})
		if health, err := p.Probe(context.Background()); !errors.Is(err, ErrTransport) || health != "" {
			t.Fatalf("Probe = %q, %v; want ErrTransport and no health state", health, err)
		}
	})

	t.Run("a cancelled context", func(t *testing.T) {
		fake := newAnthropicFake(t)
		p := newAnthropicTestProvider(t, Config{BaseURL: fake.URL})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if health, err := p.Probe(ctx); !errors.Is(err, context.Canceled) || health != "" {
			t.Fatalf("Probe = %q, %v; want context.Canceled and no health state", health, err)
		}
		if got := fake.count(); got != 0 {
			t.Errorf("a cancelled Probe sent %d request(s); it must determine nothing without touching the endpoint", got)
		}
	})
}

// TestAnthropicRefusesBeforeItSends is the global constraint as a test: a
// request the adapter cannot carry must be refused before it is written, not
// after a round trip. checkReach and checkToolMessages are the shared refusals,
// and the adapter's own validation (roles, model, cap, JSON it cannot encode)
// joins them.
func TestAnthropicRefusesBeforeItSends(t *testing.T) {
	ctx := context.Background()
	fake := newAnthropicFake(t)
	// The endpoint is configured local so the egress refusal is attributable to
	// the request's own declaration.
	p := newAnthropicTestProvider(t, Config{BaseURL: fake.URL, APIKey: "test-key", Reach: "host"})

	call := Call{ID: "toolu_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"/tmp/brief.md"}`)}
	assistant := Message{Role: "assistant", ToolCalls: []Call{call}}
	valid := func() Request {
		return Request{
			Model:     "claude-sonnet-4-5",
			MaxTokens: 256,
			Messages:  []Message{{Role: "user", Content: "Read the brief."}},
		}
	}

	cases := []struct {
		name   string
		mutate func(*Request)
		want   error
	}{
		{
			name: "a tool result with no CallID",
			mutate: func(r *Request) {
				r.Messages = []Message{assistant, {Role: "tool", Name: "read_file", Content: "x"}}
			},
			want: ErrBadRequest,
		},
		{
			name: "a tool result answering an unknown call",
			mutate: func(r *Request) {
				r.Messages = []Message{assistant, {Role: "tool", Name: "read_file", CallID: "toolu_2", Content: "x"}}
			},
			want: ErrBadRequest,
		},
		{
			name: "a tool result whose call has no preceding assistant turn",
			mutate: func(r *Request) {
				r.Messages = []Message{{Role: "tool", Name: "read_file", CallID: "toolu_1", Content: "x"}, assistant}
			},
			want: ErrBadRequest,
		},
		{
			name: "a system-role message turn",
			mutate: func(r *Request) {
				r.System = "You are a careful worker."
				r.Messages = []Message{{Role: "system", Content: "Read the brief."}}
			},
			want: ErrBadRequest,
		},
		{
			name:   "a message with no role",
			mutate: func(r *Request) { r.Messages = []Message{{Content: "x"}} },
			want:   ErrBadRequest,
		},
		{
			name:   "a request that names no model",
			mutate: func(r *Request) { r.Model = "" },
			want:   ErrBadRequest,
		},
		{
			name:   "a request that sets no reply cap",
			mutate: func(r *Request) { r.MaxTokens = 0 },
			want:   ErrBadRequest,
		},
		{
			name: "tool calls on a turn that cannot emit them",
			mutate: func(r *Request) {
				r.Messages[0].ToolCalls = []Call{{ID: "toolu_1", Name: "read_file", Arguments: json.RawMessage(`{}`)}}
			},
			want: ErrBadRequest,
		},
		{
			name: "an assistant call with no id",
			mutate: func(r *Request) {
				// No tool result, so checkToolMessages has nothing to refuse: this
				// is the adapter's own empty-id refusal, and without it the call
				// would reach the wire for a real server to reject.
				r.Messages = []Message{{Role: "assistant", ToolCalls: []Call{{Name: "read_file", Arguments: json.RawMessage(`{}`)}}}}
			},
			want: ErrBadRequest,
		},
		{
			name: "an assistant call whose arguments are not a JSON object",
			mutate: func(r *Request) {
				r.Messages = []Message{{Role: "assistant", ToolCalls: []Call{{ID: "toolu_1", Name: "read_file", Arguments: json.RawMessage(`{"path":`)}}}}
			},
			want: ErrBadRequest,
		},
		{
			name: "a tool definition with no name",
			mutate: func(r *Request) {
				r.Tools = []ToolDef{{Schema: json.RawMessage(`{"type":"object"}`)}}
			},
			want: ErrBadRequest,
		},
		{
			name: "a tool schema that is not JSON",
			mutate: func(r *Request) {
				r.Tools = []ToolDef{{Name: "read_file", Schema: json.RawMessage(`{`)}}
			},
			want: ErrBadRequest,
		},
		{
			name: "an empty user turn",
			mutate: func(r *Request) {
				r.Messages = []Message{{Role: "user"}}
			},
			want: ErrBadRequest,
		},
		{
			name: "a local-only request that declares wan",
			mutate: func(r *Request) {
				r.RequireLocal = true
				r.Reach = "wan"
			},
			want: ErrEgressRefused,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := fake.count()
			req := valid()
			tc.mutate(&req)
			resp, err := p.Complete(ctx, req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Complete = %+v, %v; want an error satisfying errors.Is(err, %v)", resp, err, tc.want)
			}
			if !zeroResponse(resp) {
				t.Errorf("a refused request returned a response: %+v", resp)
			}
			if after := fake.count(); after != before {
				t.Fatalf("the refusal issued %d request(s); a provider that cannot send must refuse before it writes the request", after-before)
			}
		})
	}

	t.Run("a cancelled context", func(t *testing.T) {
		before := fake.count()
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := p.Complete(cancelled, valid())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Complete with a cancelled context = %v, want context.Canceled", err)
		}
		if errors.Is(err, ErrTransport) {
			t.Fatalf("Complete with a cancelled context = %v; a cancellation is not a transport failure, and the retry policy reads that sentinel", err)
		}
		if after := fake.count(); after != before {
			t.Errorf("a cancelled Complete issued %d request(s)", after-before)
		}
	})

	// Fail-closed: an adapter built without an endpoint reach cannot prove the
	// endpoint is local, so a request that merely claims host must be refused.
	unconfigured := newAnthropicTestProvider(t, Config{BaseURL: fake.URL})
	before := fake.count()
	req := valid()
	req.RequireLocal = true
	req.Reach = "host"
	if _, err := unconfigured.Complete(ctx, req); !errors.Is(err, ErrEgressRefused) {
		t.Fatalf("an adapter with no configured reach accepted a local-only request: %v", err)
	}
	if after := fake.count(); after != before {
		t.Errorf("the fail-closed refusal issued %d request(s)", after-before)
	}

	// The control: a valid request does reach the endpoint, so the assertions
	// above are about refusal rather than about a provider that never sends.
	if _, err := p.Complete(ctx, valid()); err != nil {
		t.Fatalf("the control request failed: %v", err)
	}
	if got := fake.count(); got != 1 {
		t.Fatalf("the control request produced %d requests, want 1", got)
	}
}

// TestAnthropicKeepsContextEndingsOutOfTheTransportSentinel pins the two
// observation points in do: the round trip and the body read. A caller's
// cancellation, an in-flight cancel and a Config.Timeout expiry all come back as
// their own context error, never as ErrTransport, because the retry policy has
// to tell "the process is shutting down" from "the transport broke".
func TestAnthropicKeepsContextEndingsOutOfTheTransportSentinel(t *testing.T) {
	t.Run("a caller's in-flight cancellation", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-release
		}))
		t.Cleanup(func() {
			unblock()
			srv.Close()
		})
		p := newAnthropicTestProvider(t, Config{BaseURL: srv.URL, Timeout: 30 * time.Second})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := p.Complete(ctx, Request{
				Model:     "claude-sonnet-4-5",
				MaxTokens: 16,
				Messages:  []Message{{Role: "user", Content: "Read the brief."}},
			})
			done <- err
		}()

		<-started
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Complete = %v; want context.Canceled", err)
			}
			if errors.Is(err, ErrTransport) {
				t.Fatalf("Complete = %v; a cancellation must not also satisfy ErrTransport", err)
			}
		case <-time.After(5 * time.Second):
			unblock()
			t.Fatal("Complete did not return after its context was cancelled")
		}
		unblock()
	})

	t.Run("the adapter's own timeout", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-release
		}))
		t.Cleanup(func() {
			unblock()
			srv.Close()
		})
		p := newAnthropicTestProvider(t, Config{BaseURL: srv.URL, Timeout: 50 * time.Millisecond})

		done := make(chan error, 1)
		go func() {
			_, err := p.Complete(context.Background(), Request{
				Model:     "claude-sonnet-4-5",
				MaxTokens: 16,
				Messages:  []Message{{Role: "user", Content: "Read the brief."}},
			})
			done <- err
		}()
		<-started
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Complete = %v; want context.DeadlineExceeded", err)
			}
			if errors.Is(err, ErrTransport) {
				t.Fatalf("Complete = %v; a timeout must not also satisfy ErrTransport", err)
			}
		case <-time.After(5 * time.Second):
			unblock()
			t.Fatal("Complete did not return after its own timeout")
		}
		unblock()
	})

	// The body-read observation point: the response headers arrive, then the
	// stream stalls. This is the second place an error can come back, and a
	// cancellation there must also stay out of ErrTransport (the OpenAI review
	// left this branch untested).
	t.Run("a cancellation while reading the body", func(t *testing.T) {
		headersSent := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{"id":"msg_partial","type":"message"`)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			close(headersSent)
			<-r.Context().Done()
		}))
		t.Cleanup(srv.Close)
		p := newAnthropicTestProvider(t, Config{BaseURL: srv.URL, Timeout: 30 * time.Second})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := p.Complete(ctx, Request{
				Model:     "claude-sonnet-4-5",
				MaxTokens: 16,
				Messages:  []Message{{Role: "user", Content: "Read the brief."}},
			})
			done <- err
		}()
		<-headersSent
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Complete = %v; want context.Canceled, because the cancellation ended the body read", err)
			}
			if errors.Is(err, ErrTransport) {
				t.Fatalf("Complete = %v; a body-read cancellation must not also satisfy ErrTransport", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Complete did not return after the body read was cancelled")
		}
	})
}

// TestAnthropicIsRegisteredThroughTheRegistry is the wiring half: the adapter is
// one file plus one registry line, and without the line an endpoint row naming
// "anthropic" could never be constructed. It also exercises Registry.New's
// Name() assertion against the real adapter.
func TestAnthropicIsRegisteredThroughTheRegistry(t *testing.T) {
	fake := newAnthropicFake(t)
	p, err := NewRegistry().New(Config{Provider: "anthropic", BaseURL: fake.URL, APIKey: "test-key"})
	if err != nil {
		t.Fatalf("New(anthropic): %v", err)
	}
	if got := p.Name(); got != "anthropic" {
		t.Errorf("registry-constructed provider is named %q, want anthropic", got)
	}
	if _, err := p.Complete(context.Background(), Request{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 16,
		Messages:  []Message{{Role: "user", Content: "Read the brief."}},
	}); err != nil {
		t.Fatalf("the registry-constructed adapter could not complete: %v", err)
	}
	if _, err := NewRegistry().New(Config{Provider: "anthropic", BaseURL: ""}); err == nil {
		t.Fatal("New(anthropic) with no base URL succeeded; the endpoint row is unusable")
	}
}

// TestNewAnthropicValidatesItsEndpoint covers the constructor: an endpoint that
// cannot be addressed is a configuration error at construction, not a transport
// failure on every call. The API root is written with and without its /v1 in the
// wild; both spellings name the same endpoint.
func TestNewAnthropicValidatesItsEndpoint(t *testing.T) {
	for _, tc := range []struct{ name, base string }{
		{"no base URL", ""},
		{"a URL with no scheme", "127.0.0.1:8080"},
		{"a scheme that is not HTTP", "ftp://example.test"},
		{"a URL with no host", "http://"},
		{"a base URL with a query", "http://example.test?key=1"},
		{"a base URL with a fragment", "http://example.test/#x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if p, err := NewAnthropic(Config{BaseURL: tc.base}); err == nil {
				t.Fatalf("NewAnthropic(%q) = %v, nil; want an error", tc.base, p)
			}
		})
	}

	p, err := NewAnthropic(Config{BaseURL: "http://example.test"})
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	if got := p.Name(); got != "anthropic" {
		t.Errorf("Name() = %q, want anthropic", got)
	}

	for _, suffix := range []string{"", "/", "/v1", "/v1/"} {
		fake := newAnthropicFake(t)
		q := newAnthropicTestProvider(t, Config{BaseURL: fake.URL + suffix})
		if _, err := q.Complete(context.Background(), Request{
			Model:     "claude-sonnet-4-5",
			MaxTokens: 16,
			Messages:  []Message{{Role: "user", Content: "Read the brief."}},
		}); err != nil {
			t.Fatalf("base URL %q: %v", fake.URL+suffix, err)
		}
		sent := fake.requests()
		if len(sent) != 1 || sent[0].Path != "/v1/messages" {
			t.Errorf("base URL %q produced %q, want POST /v1/messages", fake.URL+suffix, sent[0].Path)
		}
	}
}

// TestAnthropicEstimateTokensIsThePackageHeuristic: this family has no tokenizer
// in the standard library and no dependency is allowed, so the adapter uses the
// package's documented heuristic rather than inventing a second one. Check 4 is
// the band assertion; this keeps the adapter wired to the shared estimator.
func TestAnthropicEstimateTokensIsThePackageHeuristic(t *testing.T) {
	p, err := NewAnthropic(Config{BaseURL: "http://example.test"})
	if err != nil {
		t.Fatalf("NewAnthropic: %v", err)
	}
	req := Request{
		System:    "you are a careful worker",
		Messages:  []Message{{Role: "user", Content: "read the brief and plan the work"}},
		Tools:     []ToolDef{{Name: "read_file", Description: "read a file", Schema: json.RawMessage(`{"type":"object"}`)}},
		MaxTokens: 256,
	}
	if got, want := p.EstimateTokens(req), estimateTokens(req); got != want {
		t.Fatalf("EstimateTokens = %d, want the package heuristic's %d", got, want)
	}
	if got := p.EstimateTokens(req); got <= 0 {
		t.Errorf("EstimateTokens = %d for a non-empty request, want a positive count", got)
	}
	if got := p.EstimateTokens(Request{}); got != 0 {
		t.Errorf("EstimateTokens(empty) = %d, want 0", got)
	}
}
