package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ollamaTagsBody is a real GET /api/tags answer: the model-list route the Ollama
// adapter probes instead of OpenAI's /v1/models. The details block is what makes
// this a fixture rather than a stub — a probe that decoded the whole row strictly
// would be pinned to today's set of detail fields, and Ollama has added to them
// (families came after family).
const ollamaTagsBody = `{
  "models": [
    {
      "name": "llama3.2:latest",
      "model": "llama3.2:latest",
      "modified_at": "2026-01-02T15:04:05.123456Z",
      "size": 2019393189,
      "digest": "sha256:a80c4f17acd55265feec403c7aef86be0c25983ab279d83f3bcd3abbcb5b8b72",
      "details": {
        "format": "gguf",
        "family": "llama",
        "families": ["llama"],
        "parameter_size": "3.2B",
        "quantization_level": "Q4_K_M"
      }
    }
  ]
}`

// ollamaFake is one server with two faces: the OpenAI fixture's chat and /v1/models
// routes unchanged, plus Ollama's GET /api/tags. Reusing the chat fixture rather
// than restating the wire shape is the point — an Ollama adapter must speak
// exactly the shape the OpenAI adapter speaks, and a second, independently
// written fixture would be free to agree with a translation mistake the two
// adapters share.
type ollamaFake struct {
	*openAIFake

	// tagsStatus and tagsBody are guarded by the embedded fake's mutex, so a test
	// that changes them between requests cannot race the handler.
	tagsStatus int
	tagsBody   string
}

func newOllamaFake(t *testing.T) *ollamaFake {
	t.Helper()
	f := &ollamaFake{
		openAIFake: &openAIFake{},
		tagsStatus: http.StatusOK,
		tagsBody:   ollamaTagsBody,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	f.URL = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

// answerTags makes GET /api/tags answer status with body from now on.
func (f *ollamaFake) answerTags(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tagsStatus, f.tagsBody = status, body
}

func (f *ollamaFake) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/api/tags" {
		f.mu.Lock()
		status, body := f.tagsStatus, f.tagsBody
		f.sent = append(f.sent, openAIFakeRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
		})
		f.mu.Unlock()
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
		return
	}
	f.openAIFake.serveHTTP(w, r)
}

func newOllamaTestProvider(t *testing.T, cfg Config) *Ollama {
	t.Helper()
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	p, err := NewOllama(cfg)
	if err != nil {
		t.Fatalf("NewOllama(%+v): %v", cfg, err)
	}
	return p
}

// TestOllamaPassesConformance is the suite's verdict. The provider is built
// directly, with no endpoint reach, which is what check 6 expects.
func TestOllamaPassesConformance(t *testing.T) {
	fake := newOllamaFake(t)
	Conformance(t, func() Provider {
		p, err := NewOllama(Config{BaseURL: fake.URL, APIKey: "test-key", Timeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("NewOllama: %v", err)
		}
		return p
	})
}

// TestOllamaReportsWarmingWhileAModelLoads is the test this task exists for. A
// local server that is loading a model answers 503 until it is ready; that is not
// a broken endpoint, and collapsing it to down removes the node from placement
// for the whole load — which on a Raspberry Pi or a laptop is the common case.
//
// The distinction is not the status: every case below is a 503 (or a later
// check's status). It is an affirmative in-progress signal in a JSON error body,
// with no failure signal beside it. A 503 whose body gives nothing to go on is
// down: a dead endpoint marked warming is silent and stays dead, while a loading
// endpoint marked down is visible, alerted, and fixes itself on the next probe.
func TestOllamaReportsWarmingWhileAModelLoads(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   Health
	}{
		{
			name:   "llama.cpp's loading envelope",
			status: http.StatusServiceUnavailable,
			body:   `{"error":{"code":503,"message":"Loading model","type":"unavailable_error"}}`,
			want:   HealthWarming,
		},
		{
			name:   "an ollama-shaped loading error",
			status: http.StatusServiceUnavailable,
			body:   `{"error":"model \"llama3.2:latest\" is loading"}`,
			want:   HealthWarming,
		},
		{
			name:   "a runner that is still warming up",
			status: http.StatusServiceUnavailable,
			body:   `{"error":"the runner is warming up: llama3.2:latest"}`,
			want:   HealthWarming,
		},
		{
			name:   "a model that is starting",
			status: http.StatusServiceUnavailable,
			body:   `{"error":"upstream model llama3.2:latest is starting"}`,
			want:   HealthWarming,
		},
		{
			name:   "a model that is not ready yet",
			status: http.StatusServiceUnavailable,
			body:   `{"error":"model llama3.2:latest is not ready yet"}`,
			want:   HealthWarming,
		},
		{
			name:   "a load that failed",
			status: http.StatusServiceUnavailable,
			body:   `{"error":"failed to load model llama3.2:latest"}`,
			want:   HealthDown,
		},
		{
			name:   "an error while loading",
			status: http.StatusServiceUnavailable,
			body:   `{"error":"error loading model weights: out of memory"}`,
			want:   HealthDown,
		},
		{
			name:   "an empty body",
			status: http.StatusServiceUnavailable,
			body:   ``,
			want:   HealthDown,
		},
		{
			name:   "a reverse proxy's HTML page",
			status: http.StatusServiceUnavailable,
			body:   `<html><head><script src="loading.js"></script></head><body>503 Service Temporarily Unavailable</body></html>`,
			want:   HealthDown,
		},
		{
			name:   "a null error",
			status: http.StatusServiceUnavailable,
			body:   `{"error":null}`,
			want:   HealthDown,
		},
		{
			name:   "a full request queue",
			status: http.StatusServiceUnavailable,
			body:   `{"error":"server busy, please try again. maximum queue of pending requests exceeded"}`,
			want:   HealthDown,
		},
		{
			name:   "a proxy's JSON error",
			status: http.StatusServiceUnavailable,
			body:   `{"error":"no healthy upstream"}`,
			want:   HealthDown,
		},
		{
			name:   "a plain-text loading body",
			status: http.StatusServiceUnavailable,
			body:   `Loading model`,
			want:   HealthDown,
		},
		{
			name:   "a 500 that says loading",
			status: http.StatusInternalServerError,
			body:   `{"error":"Loading model"}`,
			want:   HealthDown,
		},
		{
			name:   "a 502 from the gateway",
			status: http.StatusBadGateway,
			body:   `{"error":"bad gateway"}`,
			want:   HealthDown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newOllamaFake(t)
			fake.answerTags(tc.status, tc.body)
			p := newOllamaTestProvider(t, Config{BaseURL: fake.URL})

			health, err := p.Probe(context.Background())
			if err != nil {
				t.Fatalf("Probe = %q, %v; a 503 is an answer, so it determines a state", health, err)
			}
			if health != tc.want {
				if tc.want == HealthWarming {
					t.Fatalf("Probe = %q, want warming, not down: a model loading is not a broken endpoint (body %s)", health, bodyOrEmpty(tc.body))
				}
				t.Fatalf("Probe = %q, want %q (body %s)", health, tc.want, bodyOrEmpty(tc.body))
			}

			// Probing must not send a completion: the canary would load the very
			// model the probe is trying to observe, and on a Pi or laptop that
			// turns every probe into a load.
			for _, sent := range fake.requests() {
				if sent.Method != http.MethodGet || sent.Path != "/api/tags" {
					t.Errorf("Probe sent %s %s; a probe asks the tags route and nothing else", sent.Method, sent.Path)
				}
			}
			if got := fake.count(); got != 1 {
				t.Errorf("Probe sent %d requests, want 1 (GET /api/tags)", got)
			}
		})
	}
}

// bodyOrEmpty keeps the failure message readable when the fixture body is empty.
func bodyOrEmpty(body string) string {
	if body == "" {
		return "(empty)"
	}
	return body
}

// TestOllamaProbeListsModelsFromTags is the second named test: the probe's
// evidence is Ollama's GET /api/tags, not OpenAI's /v1/models, and the list it
// gets back is read loosely — the route answers with detail fields this adapter
// has no use for, and a probe that pinned them would break on the next Ollama
// release.
func TestOllamaProbeListsModelsFromTags(t *testing.T) {
	t.Run("a server with a model", func(t *testing.T) {
		fake := newOllamaFake(t)
		p := newOllamaTestProvider(t, Config{BaseURL: fake.URL})

		health, err := p.Probe(context.Background())
		if err != nil || health != HealthOK {
			t.Fatalf("Probe = %q, %v; want ok, nil", health, err)
		}
		sent := fake.requests()
		if len(sent) != 1 {
			t.Fatalf("Probe sent %d requests, want 1", len(sent))
		}
		if sent[0].Method != http.MethodGet || sent[0].Path != "/api/tags" {
			t.Errorf("Probe request = %s %s, want GET /api/tags (not /v1/models)", sent[0].Method, sent[0].Path)
		}
		if got := sent[0].Header.Get("Authorization"); got != "" {
			t.Errorf("Probe Authorization = %q with no key configured, want no header", got)
		}
	})

	t.Run("a key is carried when one is configured", func(t *testing.T) {
		// Ollama itself has no keys, but it is commonly served behind a proxy that
		// does, and the adapter's Complete carries the key, so the probe must too.
		fake := newOllamaFake(t)
		p := newOllamaTestProvider(t, Config{BaseURL: fake.URL, APIKey: "proxy-key"})
		if health, err := p.Probe(context.Background()); err != nil || health != HealthOK {
			t.Fatalf("Probe = %q, %v; want ok, nil", health, err)
		}
		if got := fake.requests()[0].Header.Get("Authorization"); got != "Bearer proxy-key" {
			t.Errorf("Probe Authorization = %q, want the configured key", got)
		}
	})

	t.Run("a server with no models", func(t *testing.T) {
		// Reachable and healthy, just empty: health is a fact about the endpoint,
		// and placement already refuses a model row that is not there.
		fake := newOllamaFake(t)
		fake.answerTags(http.StatusOK, `{"models":[]}`)
		p := newOllamaTestProvider(t, Config{BaseURL: fake.URL})
		if health, err := p.Probe(context.Background()); err != nil || health != HealthOK {
			t.Fatalf("Probe = %q, %v; want ok, nil", health, err)
		}
	})

	t.Run("detail fields this adapter does not know", func(t *testing.T) {
		fake := newOllamaFake(t)
		fake.answerTags(http.StatusOK, `{
			"models": [{
				"name": "qwen2.5:7b",
				"model": "qwen2.5:7b",
				"future_field": {"nested": [1, 2, 3]},
				"details": {"format": "gguf", "capabilities": ["tools", "vision"]}
			}],
			"next_cursor": "a field a later release added"
		}`)
		p := newOllamaTestProvider(t, Config{BaseURL: fake.URL})
		if health, err := p.Probe(context.Background()); err != nil || health != HealthOK {
			t.Fatalf("Probe = %q, %v; want ok, nil — the probe reads the models it needs and ignores the rest", health, err)
		}
	})

	t.Run("an openai models list instead of tags", func(t *testing.T) {
		// An OpenAI body means the endpoint row points at the wrong route or the
		// wrong server. It is not a tags list, so it determines no state.
		fake := newOllamaFake(t)
		fake.answerTags(http.StatusOK, `{"object":"list","data":[{"id":"gpt-4o-mini","object":"model"}]}`)
		p := newOllamaTestProvider(t, Config{BaseURL: fake.URL})
		if health, err := p.Probe(context.Background()); !errors.Is(err, ErrServer) || health != "" {
			t.Fatalf("Probe = %q, %v; want ErrServer and no state for a body that is not a tags list", health, err)
		}
	})

	t.Run("a web page", func(t *testing.T) {
		fake := newOllamaFake(t)
		fake.answerTags(http.StatusOK, `<html><body>a web page, not an API</body></html>`)
		p := newOllamaTestProvider(t, Config{BaseURL: fake.URL})
		if health, err := p.Probe(context.Background()); !errors.Is(err, ErrServer) || health != "" {
			t.Fatalf("Probe = %q, %v; want ErrServer and no state for a 200 that is not a tags list", health, err)
		}
	})

	t.Run("a null model list", func(t *testing.T) {
		// The route promises an array; null is not one, and accepting it would make
		// any JSON body with a models key look like this route.
		fake := newOllamaFake(t)
		fake.answerTags(http.StatusOK, `{"models":null}`)
		p := newOllamaTestProvider(t, Config{BaseURL: fake.URL})
		if health, err := p.Probe(context.Background()); !errors.Is(err, ErrServer) || health != "" {
			t.Fatalf("Probe = %q, %v; want ErrServer and no state", health, err)
		}
	})
}

// TestOllamaReadsModelNamesFromTags pins what "lists models" means: the names are
// read from the row's name (or its model spelling, which older releases used), and
// nothing else in the row is required to decode.
func TestOllamaReadsModelNamesFromTags(t *testing.T) {
	names, ok := ollamaTagsModels([]byte(ollamaTagsBody))
	if !ok || len(names) != 1 || names[0] != "llama3.2:latest" {
		t.Fatalf("ollamaTagsModels(tags) = %v, %v; want [llama3.2:latest], true", names, ok)
	}

	cases := []struct {
		name string
		body string
		ok   bool
		want []string
	}{
		{"an empty list", `{"models":[]}`, true, nil},
		{"a row with only the model spelling", `{"models":[{"model":"gemma3:4b"}]}`, true, []string{"gemma3:4b"}},
		{"a row with neither name nor model", `{"models":[{"digest":"sha256:x"}]}`, true, nil},
		{"a body with no models field", `{"object":"list","data":[]}`, false, nil},
		{"a null list", `{"models":null}`, false, nil},
		{"an entry that is not an object", `{"models":[41]}`, false, nil},
		{"not JSON at all", `Loading model`, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			names, ok := ollamaTagsModels([]byte(tc.body))
			if ok != tc.ok {
				t.Fatalf("ollamaTagsModels(%s) ok = %v, want %v", tc.body, ok, tc.ok)
			}
			if strings.Join(names, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("ollamaTagsModels(%s) = %v, want %v", tc.body, names, tc.want)
			}
		})
	}
}

// TestOllamaProbeNamesTheStatesItCanName collects the answers that are not a 5xx.
// Each one is a state the operator must act on differently, or an error that
// determined no state at all.
func TestOllamaProbeNamesTheStatesItCanName(t *testing.T) {
	t.Run("a key the endpoint rejects", func(t *testing.T) {
		fake := newOllamaFake(t)
		fake.answerTags(http.StatusUnauthorized, `{"error":"unauthorized"}`)
		p := newOllamaTestProvider(t, Config{BaseURL: fake.URL})
		if health, err := p.Probe(context.Background()); err != nil || health != HealthAuthFailed {
			t.Fatalf("Probe = %q, %v; want auth_failed, nil", health, err)
		}
	})

	t.Run("a route that is not there", func(t *testing.T) {
		// A 404 on /api/tags means the base URL points at something that is not
		// Ollama: a configuration error, not a machine to wait for.
		fake := newOllamaFake(t)
		fake.answerTags(http.StatusNotFound, `{"error":"404 page not found"}`)
		p := newOllamaTestProvider(t, Config{BaseURL: fake.URL})
		if health, err := p.Probe(context.Background()); !errors.Is(err, ErrModelMissing) || health != "" {
			t.Fatalf("Probe = %q, %v; want ErrModelMissing and no state", health, err)
		}
	})

	t.Run("an endpoint that is throttling", func(t *testing.T) {
		fake := newOllamaFake(t)
		fake.answerTags(http.StatusTooManyRequests, `{"error":"too many requests"}`)
		p := newOllamaTestProvider(t, Config{BaseURL: fake.URL})
		if health, err := p.Probe(context.Background()); !errors.Is(err, ErrRateLimited) || health != "" {
			t.Fatalf("Probe = %q, %v; want ErrRateLimited and no state", health, err)
		}
	})

	t.Run("a server that answers 500", func(t *testing.T) {
		fake := newOllamaFake(t)
		fake.answerTags(http.StatusInternalServerError, `{"error":"the model failed to generate a response"}`)
		p := newOllamaTestProvider(t, Config{BaseURL: fake.URL})
		if health, err := p.Probe(context.Background()); err != nil || health != HealthDown {
			t.Fatalf("Probe = %q, %v; want down, nil", health, err)
		}
	})

	t.Run("an unreachable endpoint", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		p := newOllamaTestProvider(t, Config{BaseURL: url})
		if health, err := p.Probe(context.Background()); !errors.Is(err, ErrTransport) || health != "" {
			t.Fatalf("Probe = %q, %v; want ErrTransport and no state", health, err)
		}
	})

	t.Run("a cancelled probe", func(t *testing.T) {
		fake := newOllamaFake(t)
		p := newOllamaTestProvider(t, Config{BaseURL: fake.URL})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if health, err := p.Probe(ctx); !errors.Is(err, context.Canceled) || health != "" {
			t.Fatalf("Probe = %q, %v; want context.Canceled and no state", health, err)
		}
		if got := fake.count(); got != 0 {
			t.Errorf("a cancelled Probe sent %d request(s); it must determine nothing without touching the endpoint", got)
		}
	})
}

// TestOllamaInheritsTheSharedRefusals is the wrapper's self-review as a test. The
// Ollama adapter is the OpenAI adapter composed, so checkReach, checkToolMessages
// and the request validation are the parent's calls on the parent's own state —
// not a second copy that could drift from the one the conformance suite proved.
func TestOllamaInheritsTheSharedRefusals(t *testing.T) {
	ctx := context.Background()
	fake := newOllamaFake(t)
	p := newOllamaTestProvider(t, Config{BaseURL: fake.URL, Reach: "host"})

	call := Call{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"/tmp/brief.md"}`)}
	assistant := Message{Role: "assistant", ToolCalls: []Call{call}}
	valid := func() Request {
		return Request{
			Model:     "llama3.2:latest",
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
			name: "a tool result whose call has no preceding turn",
			mutate: func(r *Request) {
				r.Messages = []Message{{Role: "tool", Name: "read_file", CallID: "call_1", Content: "x"}}
			},
			want: ErrBadRequest,
		},
		{
			name:   "a role this protocol cannot carry",
			mutate: func(r *Request) { r.Messages = []Message{{Role: "developer", Content: "x"}} },
			want:   ErrBadRequest,
		},
		{
			name:   "no model",
			mutate: func(r *Request) { r.Model = " " },
			want:   ErrBadRequest,
		},
		{
			name: "a local-only request the endpoint's reach contradicts",
			mutate: func(r *Request) {
				r.RequireLocal = true
				r.Reach = "wan"
			},
			want: ErrEgressRefused,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := valid()
			tc.mutate(&req)
			resp, err := p.Complete(ctx, req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Complete = %+v, %v; want %v", resp, err, tc.want)
			}
			if !zeroResponse(resp) {
				t.Errorf("refused Complete returned a response: %+v", resp)
			}
			if got := fake.count(); got != 0 {
				t.Errorf("a refused request sent %d request(s); it must be refused before anything is written", got)
			}
		})
	}

	t.Run("a reach the adapter never learned", func(t *testing.T) {
		// The conformance suite builds the provider with no reach; a local-only
		// request must fail closed rather than trust the caller's claim.
		q := newOllamaTestProvider(t, Config{BaseURL: fake.URL})
		req := valid()
		req.RequireLocal = true
		req.Reach = "host"
		if _, err := q.Complete(ctx, req); !errors.Is(err, ErrEgressRefused) {
			t.Fatalf("Complete = %v; want ErrEgressRefused", err)
		}
		if got := fake.count(); got != 0 {
			t.Errorf("a request with an unverifiable reach sent %d request(s)", got)
		}
	})

	t.Run("a local endpoint accepts local-only work", func(t *testing.T) {
		req := valid()
		req.RequireLocal = true
		req.Reach = "host"
		if _, err := p.Complete(ctx, req); err != nil {
			t.Fatalf("Complete on a host-reach endpoint: %v", err)
		}
	})

	t.Run("a cancelled context", func(t *testing.T) {
		before := fake.count()
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := p.Complete(cctx, valid()); !errors.Is(err, context.Canceled) {
			t.Fatalf("Complete = %v; want context.Canceled", err)
		}
		if got := fake.count(); got != before {
			t.Errorf("a cancelled Complete sent %d request(s)", got-before)
		}
	})
}

// TestOllamaSpeaksTheOpenAICompatibleChatPath pins the composition: the chat
// translation is the OpenAI adapter's, at its path, and the two roots Ollama
// writes are both accepted — "http://host:11434" and the /v1 spelling an
// OpenAI-compatible client uses.
func TestOllamaSpeaksTheOpenAICompatibleChatPath(t *testing.T) {
	for _, suffix := range []string{"", "/", "/v1", "/v1/"} {
		t.Run("base URL"+suffix, func(t *testing.T) {
			fake := newOllamaFake(t)
			p := newOllamaTestProvider(t, Config{BaseURL: fake.URL + suffix})

			req := Request{
				Model:     "llama3.2:latest",
				MaxTokens: 16,
				Messages:  []Message{{Role: "user", Content: "Read the brief."}},
				Tools: []ToolDef{{
					Name:        "read_file",
					Description: "Read a file.",
					Schema:      json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
				}},
			}
			resp, err := p.Complete(context.Background(), req)
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "read_file" {
				t.Fatalf("Complete = %+v; want the fake's tool call decoded", resp)
			}

			sent := fake.requests()
			var chat *openAIFakeRequest
			for i := range sent {
				if sent[i].Method == http.MethodPost {
					chat = &sent[i]
				}
			}
			if chat == nil || chat.Path != "/v1/chat/completions" {
				t.Fatalf("base URL %q did not reach POST /v1/chat/completions: %+v", suffix, sent)
			}

			// The chat body is the OpenAI shape the fixture validates, so reaching
			// the fake's answer at all is the assertion that the translation was
			// inherited; this one field keeps it from being a coincidence.
			var wire fakeWireRequest
			if err := json.Unmarshal(chat.Body, &wire); err != nil {
				t.Fatalf("the chat body is not the OpenAI shape: %v", err)
			}
			if wire.Model != "llama3.2:latest" || len(wire.Tools) != 1 {
				t.Errorf("chat body = %+v, want the model and one function tool", wire)
			}

			if health, err := p.Probe(context.Background()); err != nil || health != HealthOK {
				t.Fatalf("Probe = %q, %v; want ok, nil", health, err)
			}
			last := fake.requests()
			if got := last[len(last)-1]; got.Method != http.MethodGet || got.Path != "/api/tags" {
				t.Errorf("Probe request = %s %s, want GET /api/tags", got.Method, got.Path)
			}
		})
	}
}

func TestNewOllamaValidatesItsEndpoint(t *testing.T) {
	for _, tc := range []struct{ name, base string }{
		{"no base URL", ""},
		{"a URL with no scheme", "127.0.0.1:11434"},
		{"a scheme that is not HTTP", "ftp://example.test"},
		{"a URL with no host", "http://"},
		{"a base URL with a query", "http://example.test?key=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if p, err := NewOllama(Config{BaseURL: tc.base}); err == nil {
				t.Fatalf("NewOllama(%q) = %v, nil; want an error", tc.base, p)
			}
		})
	}

	p, err := NewOllama(Config{BaseURL: "http://example.test"})
	if err != nil {
		t.Fatalf("NewOllama: %v", err)
	}
	if got := p.Name(); got != "ollama" {
		t.Errorf("Name() = %q, want ollama", got)
	}
}

func TestOllamaIsRegisteredThroughTheRegistry(t *testing.T) {
	fake := newOllamaFake(t)
	p, err := NewRegistry().New(Config{Provider: "ollama", BaseURL: fake.URL})
	if err != nil {
		t.Fatalf("New(ollama): %v", err)
	}
	if got := p.Name(); got != "ollama" {
		t.Errorf("registry-constructed provider is named %q, want ollama", got)
	}
	if _, err := p.Complete(context.Background(), Request{
		Model:     "llama3.2:latest",
		MaxTokens: 16,
		Messages:  []Message{{Role: "user", Content: "Read the brief."}},
	}); err != nil {
		t.Fatalf("the registry-constructed adapter could not complete: %v", err)
	}
	if health, err := p.Probe(context.Background()); err != nil || health != HealthOK {
		t.Fatalf("Probe through the registry = %q, %v; want ok, nil", health, err)
	}

	// A bad endpoint row is refused at construction, through the registry, so the
	// failure names the row rather than becoming a transport error on every run.
	if _, err := NewRegistry().New(Config{Provider: "ollama", BaseURL: ""}); err == nil {
		t.Fatal("New(ollama) with no base URL succeeded; the endpoint row is unusable")
	}
}
