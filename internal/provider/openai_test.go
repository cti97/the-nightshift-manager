package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake's wire types are deliberately not the adapter's. A fixture that
// decoded with the adapter's own structs would agree with it about every mistake
// the two share, so the fake decodes the OpenAI shape as a stranger would and
// validates it field by field.
type fakeWireRequest struct {
	Model      string            `json:"model"`
	Messages   []fakeWireMessage `json:"messages"`
	Tools      []json.RawMessage `json:"tools"`
	ToolChoice json.RawMessage   `json:"tool_choice"`
	MaxTokens  int               `json:"max_tokens"`
}

type fakeWireMessage struct {
	Role       string             `json:"role"`
	Content    json.RawMessage    `json:"content"`
	ToolCallID string             `json:"tool_call_id"`
	ToolCalls  []fakeWireToolCall `json:"tool_calls"`
}

type fakeWireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type fakeWireResponse struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Choices []fakeWireChoice `json:"choices"`
	Usage   fakeWireUsage    `json:"usage"`
}

type fakeWireChoice struct {
	Index        int                     `json:"index"`
	Message      fakeWireResponseMessage `json:"message"`
	FinishReason string                  `json:"finish_reason"`
}

type fakeWireResponseMessage struct {
	Role      string                 `json:"role"`
	Content   *string                `json:"content"`
	ToolCalls []fakeWireResponseCall `json:"tool_calls,omitempty"`
}

type fakeWireResponseCall struct {
	ID       string                   `json:"id"`
	Type     string                   `json:"type"`
	Function fakeWireResponseFunction `json:"function"`
}

type fakeWireResponseFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type fakeWireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// fakeWireFunctionObject is a tools[] entry's function object, decoded in the
// test so the assertions read like the wire rather than like the adapter.
type fakeWireFunctionObject struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openAIFakeRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// openAIFake is the httptest endpoint Conformance is run against: one stateless
// handler that speaks the OpenAI chat-completions shape, validates the wire the
// way a real server does, and records what it received so the named adapter
// tests can inspect it.
type openAIFake struct {
	URL string

	srv *httptest.Server
	mu  sync.Mutex
	// sent is every request the adapter issued, in order.
	sent []openAIFakeRequest
}

func newOpenAIFake(t *testing.T) *openAIFake {
	t.Helper()
	f := &openAIFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	f.URL = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

func (f *openAIFake) requests() []openAIFakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]openAIFakeRequest(nil), f.sent...)
}

func (f *openAIFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *openAIFake) serveHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f.mu.Lock()
	f.sent = append(f.sent, openAIFakeRequest{
		Method: r.Method,
		Path:   r.URL.Path,
		Header: r.Header.Clone(),
		Body:   body,
	})
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")

	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		io.WriteString(w, `{"object":"list","data":[{"id":"conformance-model","object":"model","owned_by":"fake"}]}`)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		fakeFailure(w, http.StatusNotFound, fmt.Sprintf("the fake speaks GET /v1/models and POST /v1/chat/completions, not %s %s", r.Method, r.URL.Path))
		return
	}

	var req fakeWireRequest
	if err := json.Unmarshal(body, &req); err != nil {
		fakeFailure(w, http.StatusBadRequest, "the request body is not JSON: "+err.Error())
		return
	}
	if problem := fakeWireProblem(req); problem != "" {
		fakeFailure(w, http.StatusBadRequest, problem)
		return
	}

	// The four reserved conformance models answer with the family's real failure
	// bodies, so check 5 exercises the adapter's mapping rather than a made-up
	// status code.
	switch req.Model {
	case ConformanceModelAuth:
		fakeFailure(w, http.StatusUnauthorized,
			`{"error":{"message":"Incorrect API key provided: sk-test.","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}`)
		return
	case ConformanceModelRateLimited:
		fakeFailure(w, http.StatusTooManyRequests,
			`{"error":{"message":"Rate limit reached for requests per min (RPM): Limit 500, Used 500, Requested 1.","type":"requests","param":null,"code":"rate_limit_exceeded"}}`)
		return
	case ConformanceModelContextOverflow:
		fakeFailure(w, http.StatusBadRequest,
			`{"error":{"message":"This model's maximum context length is 8193 tokens. However, you requested 9000 tokens (9000 in the messages, 0 in the completion). Please reduce the length of the messages or completion.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`)
		return
	case ConformanceModelServerError:
		fakeFailure(w, http.StatusInternalServerError,
			`{"error":{"message":"The server had an error while processing your request.","type":"server_error","param":null,"code":null}}`)
		return
	}

	responded := false
	for _, m := range req.Messages {
		if m.Role == "tool" {
			responded = true
		}
	}

	// The usage is constant and positive: check 1 settles against it and check 3
	// requires two identical requests to report the same numbers.
	resp := fakeWireResponse{
		ID:     "chatcmpl-fake",
		Object: "chat.completion",
		Usage:  fakeWireUsage{PromptTokens: 37, CompletionTokens: 12, TotalTokens: 49},
	}
	choice := fakeWireChoice{Index: 0}
	if len(req.Tools) > 0 && !responded {
		choice.Message = fakeWireResponseMessage{
			Role: "assistant",
			// A real tool-call turn carries null content; the adapter must decode
			// that as no content rather than as a failure.
			Content: nil,
			ToolCalls: []fakeWireResponseCall{{
				ID:   "call_fake_1",
				Type: "function",
				Function: fakeWireResponseFunction{
					Name:      fakeFirstToolName(req.Tools),
					Arguments: `{"path":"/tmp/brief.md","limit":10}`,
				},
			}},
		}
		choice.FinishReason = "tool_calls"
	} else {
		content := "The brief asks for a careful reading of the endpoint's health."
		choice.Message = fakeWireResponseMessage{Role: "assistant", Content: &content}
		choice.FinishReason = "stop"
	}
	resp.Choices = []fakeWireChoice{choice}
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, "the fake could not encode its response: "+err.Error(), http.StatusInternalServerError)
	}
}

// fakeWireProblem is the fixture's half of the contract: it refuses a request
// that is not the OpenAI shape. Every refusal here is a translation mistake the
// suite would otherwise only notice through a downstream symptom.
func fakeWireProblem(req fakeWireRequest) string {
	for i, raw := range req.Tools {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return fmt.Sprintf("tools[%d] is not a JSON object: %v", i, err)
		}
		for key := range fields {
			if key != "type" && key != "function" {
				return fmt.Sprintf("tools[%d] carries %q; an OpenAI tool is exactly {type,function}", i, key)
			}
		}
		if _, ok := fields["type"]; !ok {
			return fmt.Sprintf("tools[%d] has no type", i)
		}
		if _, ok := fields["function"]; !ok {
			return fmt.Sprintf("tools[%d] has no function object", i)
		}
		var kind string
		if err := json.Unmarshal(fields["type"], &kind); err != nil || kind != "function" {
			return fmt.Sprintf("tools[%d].type = %s; want \"function\"", i, fields["type"])
		}
		var fn struct {
			Name        string          `json:"name"`
			Description *string         `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}
		if err := json.Unmarshal(fields["function"], &fn); err != nil {
			return fmt.Sprintf("tools[%d].function is not an object: %v", i, err)
		}
		if fn.Name == "" {
			return fmt.Sprintf("tools[%d].function has no name", i)
		}
		if len(fn.Parameters) == 0 {
			return fmt.Sprintf("tools[%d].function.parameters is missing", i)
		}
		if trimmed := strings.TrimSpace(string(fn.Parameters)); !strings.HasPrefix(trimmed, "{") {
			return fmt.Sprintf("tools[%d].function.parameters = %s; want a JSON Schema object", i, fn.Parameters)
		}
	}
	if len(req.Tools) > 0 && len(req.ToolChoice) == 0 {
		return "the request offers tools but carries no tool_choice; this fake requires the family's own field"
	}

	calls := make(map[string]bool)
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "user", "assistant", "tool":
		default:
			return fmt.Sprintf("messages[%d] has role %q, which is not an OpenAI chat role", i, m.Role)
		}
		if len(m.Content) == 0 && len(m.ToolCalls) == 0 {
			return fmt.Sprintf("messages[%d] is a %s turn with neither content nor tool_calls", i, m.Role)
		}
		if len(m.Content) > 0 {
			var content *string
			if err := json.Unmarshal(m.Content, &content); err != nil {
				return fmt.Sprintf("messages[%d].content = %s; want a JSON string or null", i, m.Content)
			}
		}
		if len(m.ToolCalls) > 0 {
			if m.Role != "assistant" {
				return fmt.Sprintf("messages[%d] is a %s turn carrying tool_calls", i, m.Role)
			}
			for j, call := range m.ToolCalls {
				if call.ID == "" {
					return fmt.Sprintf("messages[%d].tool_calls[%d] has no id", i, j)
				}
				if call.Type != "function" {
					return fmt.Sprintf("messages[%d].tool_calls[%d].type = %q; want \"function\"", i, j, call.Type)
				}
				if call.Function.Name == "" {
					return fmt.Sprintf("messages[%d].tool_calls[%d].function has no name", i, j)
				}
				var arguments string
				if err := json.Unmarshal(call.Function.Arguments, &arguments); err != nil {
					return fmt.Sprintf("messages[%d].tool_calls[%d].function.arguments = %s; OpenAI sends the arguments as a JSON string", i, j, call.Function.Arguments)
				}
				calls[call.ID] = true
			}
		}
		if m.Role == "tool" {
			if m.ToolCallID == "" {
				return fmt.Sprintf("messages[%d] is a tool turn with no tool_call_id", i)
			}
			if !calls[m.ToolCallID] {
				return fmt.Sprintf("messages[%d] answers tool_call_id %q, which no preceding assistant turn carries", i, m.ToolCallID)
			}
		}
	}
	return ""
}

// fakeFirstToolName reads the first offered tool's name. fakeWireProblem has
// already refused a request whose tools have no name, so an empty result here
// would be a hole in the fixture rather than a case to report.
func fakeFirstToolName(tools []json.RawMessage) string {
	for _, raw := range tools {
		var fields struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &fields); err == nil && fields.Function.Name != "" {
			return fields.Function.Name
		}
	}
	return ""
}

func fakeFailure(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	io.WriteString(w, body)
}

// newStatusServer answers every request with one status and body, for the error
// and probe tables. Nothing about it is OpenAI-shaped on purpose: the status and
// the body are the only inputs those tests are about.
func newStatusServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newOpenAITestProvider(t *testing.T, cfg Config) *OpenAI {
	t.Helper()
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	p, err := NewOpenAI(cfg)
	if err != nil {
		t.Fatalf("NewOpenAI(%+v): %v", cfg, err)
	}
	return p
}

// decodeJSON decodes one captured request field, failing the test rather than
// returning a zero value: a missing or misspelled field is the defect these
// assertions exist to find.
func decodeJSON[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	return v
}

func jsonEqual(t *testing.T, got, want []byte) bool {
	t.Helper()
	var gotV, wantV any
	if err := json.Unmarshal(got, &gotV); err != nil {
		t.Fatalf("decoding %s: %v", got, err)
	}
	if err := json.Unmarshal(want, &wantV); err != nil {
		t.Fatalf("decoding %s: %v", want, err)
	}
	return reflect.DeepEqual(gotV, wantV)
}

func zeroResponse(resp Response) bool {
	return resp.Content == "" && len(resp.ToolCalls) == 0 && resp.Usage == (Usage{}) && resp.Stop == ""
}

// TestOpenAIPassesConformance is the suite's verdict on the adapter, against a
// fake endpoint that speaks the real OpenAI shape. The provider is built
// directly and with no endpoint reach, which is what check 6 expects.
func TestOpenAIPassesConformance(t *testing.T) {
	fake := newOpenAIFake(t)
	Conformance(t, func() Provider {
		p, err := NewOpenAI(Config{BaseURL: fake.URL, APIKey: "test-key", Timeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("NewOpenAI: %v", err)
		}
		return p
	})
}

// TestOpenAISendsToolsAsFunctionObjects is the first named adapter test. The
// provider interface's ToolDef has name/description/schema; OpenAI wraps the
// same facts as {"type":"function","function":{"name","description",
// "parameters"}}. Sending the interface's shape would be accepted by nothing, so
// this asserts the translation both ways: definitions out, and the assistant
// turn's calls plus the tool result back.
func TestOpenAISendsToolsAsFunctionObjects(t *testing.T) {
	ctx := context.Background()
	fake := newOpenAIFake(t)
	p := newOpenAITestProvider(t, Config{BaseURL: fake.URL, APIKey: "test-key"})
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
	req := Request{
		Model:     "gpt-4o-mini",
		System:    "You are a careful night-shift worker.",
		MaxTokens: 128,
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

	sent := fake.requests()
	if len(sent) != 1 {
		t.Fatalf("the adapter sent %d requests, want 1", len(sent))
	}
	if sent[0].Method != http.MethodPost || sent[0].Path != "/v1/chat/completions" {
		t.Fatalf("request = %s %s, want POST /v1/chat/completions", sent[0].Method, sent[0].Path)
	}
	if got := sent[0].Header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want the configured key", got)
	}
	if got := sent[0].Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	body := decodeJSON[map[string]json.RawMessage](t, sent[0].Body)
	if got := decodeJSON[string](t, body["model"]); got != "gpt-4o-mini" {
		t.Errorf("model = %q, want gpt-4o-mini", got)
	}
	if got := decodeJSON[int](t, body["max_tokens"]); got != 128 {
		t.Errorf("max_tokens = %d, want 128", got)
	}
	if got := decodeJSON[string](t, body["tool_choice"]); got != "auto" {
		t.Errorf("tool_choice = %q, want auto", got)
	}

	messages := decodeJSON[[]map[string]json.RawMessage](t, body["messages"])
	if len(messages) != 2 {
		t.Fatalf("messages has %d entries, want a system turn and a user turn: %s", len(messages), body["messages"])
	}
	if got := decodeJSON[string](t, messages[0]["role"]); got != "system" {
		t.Errorf("messages[0].role = %q, want system", got)
	}
	if got := decodeJSON[string](t, messages[0]["content"]); got != req.System {
		t.Errorf("messages[0].content = %q, want the request's system prompt", got)
	}
	if got := decodeJSON[string](t, messages[1]["role"]); got != "user" {
		t.Errorf("messages[1].role = %q, want user", got)
	}

	tools := decodeJSON[[]map[string]json.RawMessage](t, body["tools"])
	if len(tools) != 1 {
		t.Fatalf("tools has %d entries, want 1", len(tools))
	}
	if len(tools[0]) != 2 {
		t.Errorf("tools[0] has keys %v; an OpenAI tool is exactly {type,function}", tools[0])
	}
	if _, ok := tools[0]["schema"]; ok {
		t.Error("tools[0] carries the provider interface's \"schema\" field; OpenAI wants function.parameters")
	}
	if _, ok := tools[0]["name"]; ok {
		t.Error("tools[0] carries a top-level \"name\"; OpenAI wants function.name")
	}
	if got := decodeJSON[string](t, tools[0]["type"]); got != "function" {
		t.Errorf("tools[0].type = %q, want function", got)
	}
	fn := decodeJSON[fakeWireFunctionObject](t, tools[0]["function"])
	if fn.Name != "read_file" {
		t.Errorf("tools[0].function.name = %q, want read_file", fn.Name)
	}
	if fn.Description != "Read a file from the run directory and return its text." {
		t.Errorf("tools[0].function.description = %q, want the request's description", fn.Description)
	}
	if !jsonEqual(t, fn.Parameters, schema) {
		t.Errorf("tools[0].function.parameters = %s, want the request's schema %s", fn.Parameters, schema)
	}

	// The other half of the round trip: the assistant turn replays the calls it
	// emitted and the result names the call it answers, in the family's fields.
	call := Call{ID: "call_fake_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"/tmp/a\"b.md","limit":10}`)}
	req.Messages = append(req.Messages,
		Message{Role: "assistant", Content: "", ToolCalls: []Call{call}},
		Message{Role: "tool", Name: "read_file", CallID: "call_fake_1", Content: `{"content":"# The brief"}`},
	)
	if _, err := p.Complete(ctx, req); err != nil {
		t.Fatalf("Complete with a tool result: %v", err)
	}

	sent = fake.requests()
	if len(sent) != 2 {
		t.Fatalf("the adapter sent %d requests, want 2", len(sent))
	}
	body = decodeJSON[map[string]json.RawMessage](t, sent[1].Body)
	messages = decodeJSON[[]map[string]json.RawMessage](t, body["messages"])
	if len(messages) != 4 {
		t.Fatalf("the replayed conversation has %d messages, want 4: %s", len(messages), body["messages"])
	}

	assistant := messages[2]
	if got := decodeJSON[string](t, assistant["role"]); got != "assistant" {
		t.Errorf("messages[2].role = %q, want assistant", got)
	}
	replayed := decodeJSON[[]fakeWireToolCall](t, assistant["tool_calls"])
	if len(replayed) != 1 {
		t.Fatalf("the assistant turn replays %d calls, want 1: %s", len(replayed), assistant["tool_calls"])
	}
	if replayed[0].ID != call.ID || replayed[0].Type != "function" || replayed[0].Function.Name != call.Name {
		t.Errorf("replayed call = %+v, want id %q, type function, name %q", replayed[0], call.ID, call.Name)
	}
	var arguments string
	if err := json.Unmarshal(replayed[0].Function.Arguments, &arguments); err != nil {
		t.Fatalf("replayed arguments = %s, want a JSON string: %v", replayed[0].Function.Arguments, err)
	}
	if arguments != string(call.Arguments) {
		t.Errorf("replayed arguments = %s, want the call's own bytes %s", arguments, call.Arguments)
	}
	if raw, ok := assistant["content"]; ok {
		var content *string
		if err := json.Unmarshal(raw, &content); err != nil {
			t.Fatalf("messages[2].content = %s, want a string or null", raw)
		}
		if content != nil && *content != "" {
			t.Errorf("messages[2].content = %q, want no content on a tool-call turn", *content)
		}
	}

	tool := messages[3]
	if got := decodeJSON[string](t, tool["role"]); got != "tool" {
		t.Errorf("messages[3].role = %q, want tool", got)
	}
	if got := decodeJSON[string](t, tool["tool_call_id"]); got != call.ID {
		t.Errorf("messages[3].tool_call_id = %q, want %q", got, call.ID)
	}
	if got := decodeJSON[string](t, tool["content"]); got != `{"content":"# The brief"}` {
		t.Errorf("messages[3].content = %q, want the tool result", got)
	}
	if _, ok := tool["name"]; ok {
		t.Error("the tool turn carries a name; OpenAI identifies a result by tool_call_id, and strict servers reject the deprecated function-role field")
	}
}

// TestOpenAIDecodesToolCallsVerbatim is the second named adapter test. The wire
// carries the model's arguments as a JSON *string*; the tool layer will hand the
// bytes back to json.Unmarshal, so the adapter must not decode them into a map
// and re-encode them (which reorders keys) or otherwise "fix" what the model
// wrote.
func TestOpenAIDecodesToolCallsVerbatim(t *testing.T) {
	// The arguments are deliberately not in map order: a map round trip sorts
	// them to a, limit, path, z, which the assertion below would catch. The
	// escaped quote is there because the value travels inside a JSON string.
	const arguments = `{"path":"/tmp/a\"b.md","limit":10,"z":true,"a":null}`

	server := fakeCompletionServer(t, arguments)
	p := newOpenAITestProvider(t, Config{BaseURL: server.URL})
	resp, err := p.Complete(context.Background(), Request{
		Model:     "gpt-4o-mini",
		MaxTokens: 64,
		Messages:  []Message{{Role: "user", Content: "Read the brief."}},
		Tools:     []ToolDef{{Name: "read_file", Schema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("Complete returned %d tool calls, want 1", len(resp.ToolCalls))
	}
	got := resp.ToolCalls[0]
	if got.ID != "call_v" || got.Name != "read_file" {
		t.Errorf("call = %+v, want id call_v and name read_file", got)
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

	// A malformed arguments string is the model's own bytes, and the tool layer
	// reports it as a tool failure the model can answer. The adapter preserves it
	// rather than repairing it into {} or failing the whole step.
	t.Run("a malformed arguments string is preserved, not repaired", func(t *testing.T) {
		const malformed = `{"path":`
		server := fakeCompletionServer(t, malformed)
		p := newOpenAITestProvider(t, Config{BaseURL: server.URL})
		resp, err := p.Complete(context.Background(), Request{
			Model:     "gpt-4o-mini",
			MaxTokens: 64,
			Messages:  []Message{{Role: "user", Content: "Read the brief."}},
		})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if len(resp.ToolCalls) != 1 {
			t.Fatalf("Complete returned %d tool calls, want 1", len(resp.ToolCalls))
		}
		if got := string(resp.ToolCalls[0].Arguments); got != malformed {
			t.Fatalf("arguments = %q, want the model's own %q; the adapter must not repair it", got, malformed)
		}
	})

	// The leniency stops at the wire shape: the arguments must be a JSON string,
	// because that is what the protocol says. An object is a different protocol
	// pretending to be this one, and silently accepting it would hide that.
	t.Run("an arguments object is refused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_v","type":"function","function":{"name":"read_file","arguments":{"path":"/tmp/brief.md"}}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
		}))
		t.Cleanup(srv.Close)
		p := newOpenAITestProvider(t, Config{BaseURL: srv.URL})
		_, err := p.Complete(context.Background(), Request{
			Model:     "gpt-4o-mini",
			MaxTokens: 64,
			Messages:  []Message{{Role: "user", Content: "Read the brief."}},
		})
		if !errors.Is(err, ErrServer) {
			t.Fatalf("Complete = %v, want ErrServer for arguments that are not a JSON string", err)
		}
	})
}

// fakeCompletionServer answers one chat completion with a single tool call whose
// arguments are the given JSON text. Building the response with encoding/json
// means the Go string is escaped into the wire's JSON string correctly, so the
// test's expectation is the model's text rather than a hand-escaped literal.
func fakeCompletionServer(t *testing.T, arguments string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		resp := fakeWireResponse{
			ID:     "chatcmpl-verbatim",
			Object: "chat.completion",
			Choices: []fakeWireChoice{{
				Index:        0,
				FinishReason: "tool_calls",
				Message: fakeWireResponseMessage{
					Role: "assistant",
					ToolCalls: []fakeWireResponseCall{{
						ID:   "call_v",
						Type: "function",
						Function: fakeWireResponseFunction{
							Name:      "read_file",
							Arguments: arguments,
						},
					}},
				},
			}},
			Usage: fakeWireUsage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encoding the fake response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestOpenAIMapsAnErrorBodyToATaxonomySentinel is the third named adapter test.
// The status names the class; only a 400's body refines it into context overflow,
// because that is the one class OpenAI reports with 400 and a recognizable code
// or message. The 429 case is the brief's named one and is first in the table.
func TestOpenAIMapsAnErrorBodyToATaxonomySentinel(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{
			name:   "429 a real rate-limit body",
			status: http.StatusTooManyRequests,
			body:   `{"error":{"message":"Rate limit reached for requests per min (RPM): Limit 500, Used 500, Requested 1.","type":"requests","param":null,"code":"rate_limit_exceeded"}}`,
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
			body:   `{"error":{"message":"Incorrect API key provided: sk-test.","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}`,
			want:   ErrAuth,
		},
		{
			name:   "403 a region the account cannot use",
			status: http.StatusForbidden,
			body:   `{"error":{"message":"Country, region, or territory not supported","type":"invalid_request_error","code":"unsupported_country_region_territory"}}`,
			want:   ErrAuth,
		},
		{
			name:   "404 a model the endpoint does not have",
			status: http.StatusNotFound,
			body:   `{"error":{"message":"The model 'gpt-9' does not exist or you do not have access to it.","type":"invalid_request_error","param":null,"code":"model_not_found"}}`,
			want:   ErrModelMissing,
		},
		{
			name:   "400 a context-too-long body",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"This model's maximum context length is 8193 tokens. However, you requested 9000 tokens (9000 in the messages, 0 in the completion). Please reduce the length of the messages or completion.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`,
			want:   ErrContextOverflow,
		},
		{
			name:   "400 a context-too-long message with no code",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"The input exceeds the maximum context window for this model.","type":"invalid_request_error"}}`,
			want:   ErrContextOverflow,
		},
		{
			name:   "400 a malformed request of another kind",
			status: http.StatusBadRequest,
			body:   `{"error":{"message":"Invalid value for 'max_tokens': must be greater than 0","type":"invalid_request_error","param":"max_tokens","code":null}}`,
			want:   ErrBadRequest,
		},
		{
			name:   "422 an unprocessable request",
			status: http.StatusUnprocessableEntity,
			body:   `{"error":{"message":"Unprocessable","type":"invalid_request_error"}}`,
			want:   ErrBadRequest,
		},
		{
			name:   "500 a server error",
			status: http.StatusInternalServerError,
			body:   `{"error":{"message":"The server had an error while processing your request.","type":"server_error","param":null,"code":null}}`,
			want:   ErrServer,
		},
		{
			name:   "503 an overloaded server",
			status: http.StatusServiceUnavailable,
			body:   `{"error":{"message":"The engine is currently overloaded, please try again later","type":"server_error","code":null}}`,
			want:   ErrServer,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newStatusServer(t, tc.status, tc.body)
			p := newOpenAITestProvider(t, Config{BaseURL: srv.URL})
			resp, err := p.Complete(context.Background(), Request{
				Model:     "gpt-4o-mini",
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
			// The class is not the whole message: the endpoint's own text is what
			// an operator acts on.
			if body := openAIErrorText(tc.body); body != "" && !strings.Contains(err.Error(), body) {
				t.Errorf("error %q does not carry the endpoint's message %q", err, body)
			}
		})
	}

	t.Run("a transport failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		p := newOpenAITestProvider(t, Config{BaseURL: url})
		if _, err := p.Complete(context.Background(), Request{
			Model:     "gpt-4o-mini",
			MaxTokens: 16,
			Messages:  []Message{{Role: "user", Content: "Read the brief."}},
		}); !errors.Is(err, ErrTransport) {
			t.Fatalf("Complete against a closed server = %v, want ErrTransport", err)
		}
	})
}

// openAIErrorText is the message an OpenAI-shaped error body carries, or "" for a
// body that is not that shape.
func openAIErrorText(body string) string {
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

// TestOpenAIRequiresAUsageTheBudgetCanSettle pins the first judgement call. The
// seam's Usage has no "estimated" flag, so a provider that substituted its own
// estimate for an absent one would be indistinguishable from a measuring one —
// and the budget and cost accounting read Usage for every run. An endpoint that
// answers 200 without a countable usage is refused rather than settled against a
// number the adapter made up.
func TestOpenAIRequiresAUsageTheBudgetCanSettle(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{
			name: "no usage at all",
			body: `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"The brief is planned."},"finish_reason":"stop"}]}`,
			want: ErrServer,
		},
		{
			name: "a zero prompt count",
			body: `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"The brief is planned."},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":12,"total_tokens":12}}`,
			want: ErrServer,
		},
		{
			name: "a zero completion count",
			body: `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"The brief is planned."},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":0,"total_tokens":11}}`,
			want: ErrServer,
		},
		{
			name: "no choices",
			body: `{"id":"x","object":"chat.completion","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`,
			want: ErrServer,
		},
		{
			name: "a body that is not a completion",
			body: `not json`,
			want: ErrServer,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newStatusServer(t, http.StatusOK, tc.body)
			p := newOpenAITestProvider(t, Config{BaseURL: srv.URL})
			resp, err := p.Complete(context.Background(), Request{
				Model:     "gpt-4o-mini",
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
	srv := newStatusServer(t, http.StatusOK, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"The brief is planned."},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`)
	p := newOpenAITestProvider(t, Config{BaseURL: srv.URL})
	resp, err := p.Complete(context.Background(), Request{
		Model:     "gpt-4o-mini",
		MaxTokens: 16,
		Messages:  []Message{{Role: "user", Content: "Read the brief."}},
	})
	if err != nil {
		t.Fatalf("Complete with a countable usage: %v", err)
	}
	if resp.Usage != (Usage{In: 11, Out: 7}) {
		t.Errorf("Usage = %+v, want the endpoint's prompt/completion counts", resp.Usage)
	}
	if resp.Content != "The brief is planned." || resp.Stop != "stop" {
		t.Errorf("response = %+v, want the endpoint's content and finish reason", resp)
	}
}

// TestOpenAIProbeDistinguishesAMissingModelFromATransportFailure is the
// self-review question as a test. A 404 means the endpoint answered and said the
// route is not there: a configuration error, returned as ErrModelMissing with no
// health state so the board does not record it as a server that is merely down.
// An unreachable endpoint determined nothing, so it is ErrTransport with no
// state either; a 5xx *is* a health judgement and comes back as HealthDown.
func TestOpenAIProbeDistinguishesAMissingModelFromATransportFailure(t *testing.T) {
	t.Run("an endpoint that lists models", func(t *testing.T) {
		fake := newOpenAIFake(t)
		p := newOpenAITestProvider(t, Config{BaseURL: fake.URL, APIKey: "test-key"})
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
		if got := sent[0].Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Probe Authorization = %q, want the configured key", got)
		}
	})

	t.Run("a key the endpoint rejects", func(t *testing.T) {
		srv := newStatusServer(t, http.StatusUnauthorized, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`)
		p := newOpenAITestProvider(t, Config{BaseURL: srv.URL})
		health, err := p.Probe(context.Background())
		if err != nil || health != HealthAuthFailed {
			t.Fatalf("Probe = %q, %v; want auth_failed, nil", health, err)
		}
	})

	t.Run("a server that answers 500", func(t *testing.T) {
		srv := newStatusServer(t, http.StatusInternalServerError, `{"error":{"message":"The server had an error","type":"server_error"}}`)
		p := newOpenAITestProvider(t, Config{BaseURL: srv.URL})
		health, err := p.Probe(context.Background())
		if err != nil || health != HealthDown {
			t.Fatalf("Probe = %q, %v; want down, nil", health, err)
		}
	})

	t.Run("a route that does not exist", func(t *testing.T) {
		srv := newStatusServer(t, http.StatusNotFound, `{"error":{"message":"Not Found","type":"invalid_request_error"}}`)
		p := newOpenAITestProvider(t, Config{BaseURL: srv.URL})
		health, err := p.Probe(context.Background())
		if !errors.Is(err, ErrModelMissing) {
			t.Fatalf("Probe = %q, %v; want ErrModelMissing, because the endpoint answered and the route is wrong", health, err)
		}
		if health != "" {
			t.Errorf("Probe reported health %q alongside an error; a non-nil error means no state was determined", health)
		}
	})

	t.Run("an endpoint that is throttling", func(t *testing.T) {
		srv := newStatusServer(t, http.StatusTooManyRequests, `{"error":{"message":"Rate limit reached","code":"rate_limit_exceeded"}}`)
		p := newOpenAITestProvider(t, Config{BaseURL: srv.URL})
		if health, err := p.Probe(context.Background()); !errors.Is(err, ErrRateLimited) || health != "" {
			t.Fatalf("Probe = %q, %v; want ErrRateLimited and no health state", health, err)
		}
	})

	t.Run("a 200 that is not a models list", func(t *testing.T) {
		srv := newStatusServer(t, http.StatusOK, "<html><body>a web page, not an API</body></html>")
		p := newOpenAITestProvider(t, Config{BaseURL: srv.URL})
		if health, err := p.Probe(context.Background()); !errors.Is(err, ErrServer) || health != "" {
			t.Fatalf("Probe = %q, %v; want ErrServer for a 200 that is not a models list", health, err)
		}
	})

	t.Run("an unreachable endpoint", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		p := newOpenAITestProvider(t, Config{BaseURL: url})
		if health, err := p.Probe(context.Background()); !errors.Is(err, ErrTransport) || health != "" {
			t.Fatalf("Probe = %q, %v; want ErrTransport and no health state", health, err)
		}
	})

	t.Run("a cancelled context", func(t *testing.T) {
		fake := newOpenAIFake(t)
		p := newOpenAITestProvider(t, Config{BaseURL: fake.URL})
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

// TestOpenAIRefusesBeforeItSends is the global constraint as a test: a request
// the adapter cannot send must be refused before it is written, not after a
// round trip. checkReach and checkToolMessages are the shared refusals, and the
// adapter's own validation (roles, model, JSON it cannot encode) joins them.
func TestOpenAIRefusesBeforeItSends(t *testing.T) {
	ctx := context.Background()
	fake := newOpenAIFake(t)
	// The endpoint is configured local so the egress refusal is attributable to
	// the request's own declaration.
	p := newOpenAITestProvider(t, Config{BaseURL: fake.URL, APIKey: "test-key", Reach: "host"})

	call := Call{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"/tmp/brief.md"}`)}
	assistant := Message{Role: "assistant", ToolCalls: []Call{call}}
	valid := func() Request {
		return Request{
			Model:     "gpt-4o-mini",
			MaxTokens: 16,
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
				r.Messages = []Message{assistant, {Role: "tool", Name: "read_file", CallID: "call_2", Content: "x"}}
			},
			want: ErrBadRequest,
		},
		{
			name: "a tool result whose call has no preceding assistant turn",
			mutate: func(r *Request) {
				r.Messages = []Message{{Role: "tool", Name: "read_file", CallID: "call_1", Content: "x"}, assistant}
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
			name:   "a negative reply cap",
			mutate: func(r *Request) { r.MaxTokens = -1 },
			want:   ErrBadRequest,
		},
		{
			name: "tool calls on a turn that cannot emit them",
			mutate: func(r *Request) {
				r.Messages[0].ToolCalls = []Call{{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{}`)}}
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
	// This is check 6's case, asserted here against a request that would
	// otherwise be valid.
	unconfigured := newOpenAITestProvider(t, Config{BaseURL: fake.URL})
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

// TestOpenAIIsRegisteredThroughTheRegistry is the wiring half: the adapter is one
// file plus one registry line, and without the line an endpoint row naming
// "openai" could never be constructed (Task 8 validates provider names against
// this registry). It also exercises Registry.New's Name() assertion against the
// real adapter rather than a test double.
func TestOpenAIIsRegisteredThroughTheRegistry(t *testing.T) {
	fake := newOpenAIFake(t)
	p, err := NewRegistry().New(Config{Provider: "openai", BaseURL: fake.URL, APIKey: "test-key"})
	if err != nil {
		t.Fatalf("New(openai): %v", err)
	}
	if got := p.Name(); got != "openai" {
		t.Errorf("registry-constructed provider is named %q, want openai", got)
	}
	if _, err := p.Complete(context.Background(), Request{
		Model:     "gpt-4o-mini",
		MaxTokens: 16,
		Messages:  []Message{{Role: "user", Content: "Read the brief."}},
	}); err != nil {
		t.Fatalf("the registry-constructed adapter could not complete: %v", err)
	}

	// A bad endpoint row is refused at construction, through the registry, so the
	// failure names the endpoint's configuration rather than a transport error on
	// every run.
	if _, err := NewRegistry().New(Config{Provider: "openai", BaseURL: ""}); err == nil {
		t.Fatal("New(openai) with no base URL succeeded; the endpoint row is unusable")
	}
}

// TestNewOpenAIValidatesItsEndpoint covers the constructor: an endpoint that
// cannot be addressed is a configuration error at construction, not a transport
// failure on every call.
func TestNewOpenAIValidatesItsEndpoint(t *testing.T) {
	for _, tc := range []struct{ name, base string }{
		{"no base URL", ""},
		{"a URL with no scheme", "127.0.0.1:8080"},
		{"a scheme that is not HTTP", "ftp://example.test"},
		{"a URL with no host", "http://"},
		{"a base URL with a query", "http://example.test?key=1"},
		{"a base URL with a fragment", "http://example.test/#x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if p, err := NewOpenAI(Config{BaseURL: tc.base}); err == nil {
				t.Fatalf("NewOpenAI(%q) = %v, nil; want an error", tc.base, p)
			}
		})
	}

	p, err := NewOpenAI(Config{BaseURL: "http://example.test"})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
	}
	if got := p.Name(); got != "openai" {
		t.Errorf("Name() = %q, want openai", got)
	}

	// The API root is written with and without its /v1 in the wild; both
	// spellings name the same endpoint, so both must reach the same path.
	for _, suffix := range []string{"", "/", "/v1", "/v1/"} {
		fake := newOpenAIFake(t)
		q := newOpenAITestProvider(t, Config{BaseURL: fake.URL + suffix})
		if _, err := q.Complete(context.Background(), Request{
			Model:     "gpt-4o-mini",
			MaxTokens: 16,
			Messages:  []Message{{Role: "user", Content: "Read the brief."}},
		}); err != nil {
			t.Fatalf("base URL %q: %v", fake.URL+suffix, err)
		}
		sent := fake.requests()
		if len(sent) != 1 || sent[0].Path != "/v1/chat/completions" {
			t.Errorf("base URL %q produced %s, want POST /v1/chat/completions", fake.URL+suffix, sent[0].Path)
		}
	}

	// No key configured is not an empty bearer token.
	fake := newOpenAIFake(t)
	q := newOpenAITestProvider(t, Config{BaseURL: fake.URL})
	if _, err := q.Complete(context.Background(), Request{
		Model:     "gpt-4o-mini",
		MaxTokens: 16,
		Messages:  []Message{{Role: "user", Content: "Read the brief."}},
	}); err != nil {
		t.Fatalf("Complete without a key: %v", err)
	}
	if got := fake.requests()[0].Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q with no key configured, want no header", got)
	}
}

// TestOpenAIEstimateTokensIsThePackageHeuristic pins the second judgement call:
// this family has no tokenizer in the standard library, so the adapter uses the
// package's documented heuristic rather than inventing a second one. Check 4 is
// the band assertion; this test keeps the adapter wired to the shared estimator.
func TestOpenAIEstimateTokensIsThePackageHeuristic(t *testing.T) {
	p, err := NewOpenAI(Config{BaseURL: "http://example.test"})
	if err != nil {
		t.Fatalf("NewOpenAI: %v", err)
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

// TestOpenAIReplaysWhateverTheModelWrote covers the ruling on replay: the decode
// side preserves an arguments string that is not valid JSON, so the replay side
// must send it back unchanged. Otherwise the passthrough promise is unkeepable
// and a run dead-ends on exactly the local servers that return "" for a
// no-argument tool. The empty string, a truncated object and a bare word are the
// three shapes the model layer has to be able to answer with a tool error.
func TestOpenAIReplaysWhateverTheModelWrote(t *testing.T) {
	ctx := context.Background()
	cases := []struct{ name, arguments string }{
		{"the empty string a no-argument tool returns", ""},
		{"a truncated object", `{"path":`},
		{"a bare word", "not json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newOpenAIFake(t)
			p := newOpenAITestProvider(t, Config{BaseURL: fake.URL})
			call := Call{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(tc.arguments)}
			_, err := p.Complete(ctx, Request{
				Model:     "gpt-4o-mini",
				MaxTokens: 16,
				Messages: []Message{
					{Role: "user", Content: "Read the brief."},
					{Role: "assistant", ToolCalls: []Call{call}},
					{Role: "tool", Name: "read_file", CallID: "call_1", Content: `{"error":"the arguments did not parse"}`},
				},
			})
			if err != nil {
				t.Fatalf("Complete = %v; a replayed arguments string is echoed, not refused", err)
			}
			sent := fake.requests()
			if len(sent) != 1 {
				t.Fatalf("the replay sent %d requests, want 1", len(sent))
			}
			body := decodeJSON[map[string]json.RawMessage](t, sent[0].Body)
			messages := decodeJSON[[]map[string]json.RawMessage](t, body["messages"])
			calls := decodeJSON[[]fakeWireToolCall](t, messages[1]["tool_calls"])
			if len(calls) != 1 {
				t.Fatalf("the replayed assistant turn carries %d calls, want 1", len(calls))
			}
			var got string
			if err := json.Unmarshal(calls[0].Function.Arguments, &got); err != nil {
				t.Fatalf("replayed arguments = %s, want a JSON string: %v", calls[0].Function.Arguments, err)
			}
			if got != tc.arguments {
				t.Fatalf("replayed arguments = %q, want the model's own %q byte-for-byte", got, tc.arguments)
			}
		})
	}
}

// TestOpenAIReportsATimeoutAsADeadlineNotATransportFailure pins the other half of
// the single-sentinel rule: a Config.Timeout expiry is its own deadline, not an
// ErrTransport. Task 6 may retry a timeout; it must not retry a cancellation, and
// it can only tell them apart if the adapter keeps the two apart.
func TestOpenAIReportsATimeoutAsADeadlineNotATransportFailure(t *testing.T) {
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
	// Config.Timeout is the adapter's own deadline, not the caller's: the caller's
	// context has no deadline at all.
	p := newOpenAITestProvider(t, Config{BaseURL: srv.URL, Timeout: 50 * time.Millisecond})

	done := make(chan error, 1)
	go func() {
		_, err := p.Complete(context.Background(), Request{
			Model:     "gpt-4o-mini",
			MaxTokens: 16,
			Messages:  []Message{{Role: "user", Content: "Read the brief."}},
		})
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		unblock()
		t.Fatal("the request never reached the server")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Complete = %v; want an error satisfying errors.Is(err, context.DeadlineExceeded)", err)
		}
		if errors.Is(err, ErrTransport) {
			t.Fatalf("Complete = %v; a Config.Timeout expiry must not also satisfy ErrTransport", err)
		}
	case <-time.After(5 * time.Second):
		unblock()
		t.Fatal("Complete did not return after its own timeout")
	}
	unblock()
}

// TestOpenAIStopsOnACancelledContext: Complete must return promptly when its
// context is cancelled rather than leaving the request running behind it.
func TestOpenAIStopsOnACancelledContext(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	// The handler is released explicitly rather than by the request context: the
	// test is about Complete returning, and a teardown that waits on the server's
	// own context can outlive the assertion and hang Close.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}))
	t.Cleanup(func() {
		unblock()
		srv.Close()
	})
	p := newOpenAITestProvider(t, Config{BaseURL: srv.URL, Timeout: 30 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := p.Complete(ctx, Request{
			Model:     "gpt-4o-mini",
			MaxTokens: 16,
			Messages:  []Message{{Role: "user", Content: "Read the brief."}},
		})
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the server")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Complete = %v; want an error satisfying errors.Is(err, context.Canceled)", err)
		}
		if errors.Is(err, ErrTransport) {
			t.Fatalf("Complete = %v; an in-flight cancellation must not also satisfy ErrTransport, or the retry policy cannot tell a shutdown from a broken transport", err)
		}
	case <-time.After(5 * time.Second):
		unblock()
		t.Fatal("Complete did not return after its context was cancelled")
	}
	unblock()
}
