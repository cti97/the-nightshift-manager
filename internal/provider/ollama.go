package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// The Ollama adapter: the OpenAI-compatible chat-shape adapter *composed*, plus
// Ollama's own GET /api/tags route for Probe.
//
// Composition is the decision, not an accident of code golf. Ollama's chat
// endpoint is the OpenAI shape — /v1/chat/completions, function tools, a JSON
// string of arguments — so a second translation of it would be a second place for
// the tool round trip, the verbatim arguments, the usage refusal and the
// error taxonomy to drift the moment either file is fixed. The wrapper inherits
// all of them, plus the two refusals that must run before anything is written:
// Complete is the parent's method, so checkReach (with the parent's reach field)
// and checkToolMessages are the calls the conformance suite already proved, not a
// reimplementation here.
//
// What is deliberately NOT inherited, and why that is safe:
//
//   - Name. The endpoint row selects "ollama"; a wrapper that reported "openai"
//     would fail the registry's own name assertion at construction.
//   - Probe. The parent asks GET /v1/models and folds every 5xx into down. Ollama
//     answers a model list on GET /api/tags, and a 5xx while a model is loading is
//     warming, not down — the distinction this task exists for.
//   - The error-message prefix. Diagnostics raised inside the inherited Complete
//     say "provider: openai", which names the protocol family the bytes are
//     spoken in, not the product row; the probe's own errors say "provider:
//     ollama". Callers classify with errors.Is on the taxonomy sentinels and never
//     parse the message, so this is a cosmetic wart, and adding a mutable name
//     field to a just-proved adapter to fix a prefix would be the larger risk.
//     It is named in the task report rather than hidden.
//
// Everything else — endpoint validation, base-URL normalization (a trailing /v1
// is Ollama's OpenAI-compatible spelling of the same root), the per-request
// timeout, the API key, the reach, EstimateTokens — is the parent's, unchanged.
const (
	ollamaProviderName = "ollama"
	ollamaTagsPath     = "/api/tags"
)

// Ollama is the OpenAI-compatible adapter with Ollama's name and probe route. It
// is constructed once per endpoint and is safe for concurrent use for the same
// reason the parent is: every field is set at construction and never mutated.
type Ollama struct{ *OpenAI }

var _ Provider = (*Ollama)(nil)

// NewOllama builds the adapter for the endpoint in cfg. Construction is
// NewOpenAI's, so an unaddressable endpoint fails here rather than on every run,
// and the reach is carried with no default (fail-closed, as checkReach requires).
func NewOllama(cfg Config) (*Ollama, error) {
	inner, err := NewOpenAI(cfg)
	if err != nil {
		return nil, err
	}
	return &Ollama{OpenAI: inner}, nil
}

// newOllamaProvider is the registry constructor for "ollama": one protocol is one
// file plus one registry line.
func newOllamaProvider(cfg Config) (Provider, error) { return NewOllama(cfg) }

func (o *Ollama) Name() string { return ollamaProviderName }

// Probe asks the endpoint for its model list on Ollama's own route. The four
// states are the answer to "what should an operator do", and the mapping is:
//
//	2xx                    → ok, when the body is a tags list
//	401, 403               → auth_failed: reachable, credential is wrong
//	503 + a loading body   → warming: a model is on its way up; wait, do not fix
//	503 + anything else    → down
//	500, 502, 504, other 5xx → down
//	404                    → no state, ErrModelMissing: the route is not there, so
//	                         the base URL points at something that is not Ollama
//	429, other 4xx         → no state, the taxonomy sentinel
//	no answer              → no state, ErrTransport
//
// The 503 split is the whole reason this method is not inherited, and it is
// argued below (ollamaLoading). The uninformative 503 falls to down on purpose,
// for an epistemic reason rather than a safety one: a 503 whose body says nothing
// about a load is not evidence of a load, and inventing a state from silence is
// how a board starts lying about what it knows. Both states cost the same
// placement — placement requires health "ok" — and neither raises an event, so the
// only thing the label changes is what the operator reads in /why. "down" then
// says what the probe actually observed; "warming" would say something it did not.
// The load-vs-down distinction the state exists for survives where it is
// justified: a 503 that names a load in progress is warming, and one that does
// not is down.
//
// A decision this rule owes to Task 9: if the probe schedule alerts on a
// transition to down, an uninformative 503 during a cold load will page someone.
// That has to be settled when the schedule lands — either the load path gets an
// affirmative body signal from the servers this project actually runs, or the
// alerting rule treats a first down after ok differently from a sustained one.
//
// The probe lists models and nothing else: it sends no completion canary, because
// the canary would load the very model the probe is trying to observe. On a
// Raspberry Pi or a laptop that turns every probe into a load, and a probe that
// causes the condition it reports is worse than useless. A consequence is stated
// plainly: Ollama's own daemon serves /api/tags from its model store even while a
// model loads, so this route's warming signal fires for the local servers that
// take the whole API down while a model starts (llama.cpp, llama-swap, a proxy
// in front of either). The chat route is where Ollama itself would be slow, and
// it is not asked here.
func (o *Ollama) Probe(ctx context.Context) (Health, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("provider: ollama probe: %w", err)
	}
	status, body, err := o.do(ctx, http.MethodGet, ollamaTagsPath, nil)
	if err != nil {
		return "", err
	}
	switch {
	case status >= http.StatusOK && status <= 299:
		// A 2xx must still be a tags list: a probe that called any 200 healthy
		// would call a web page — or an OpenAI server on the wrong port — a live
		// Ollama endpoint.
		if _, ok := ollamaTagsModels(body); !ok {
			return "", fmt.Errorf("%w: provider: ollama probe: the tags endpoint answered HTTP %d with something that is not an Ollama model list", ErrServer, status)
		}
		return HealthOK, nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return HealthAuthFailed, nil
	case status == http.StatusServiceUnavailable:
		if ollamaLoading(body) {
			return HealthWarming, nil
		}
		return HealthDown, nil
	case status >= 500:
		return HealthDown, nil
	default:
		// Reachable and not usable, but not a health judgement: reuse the family's
		// shared status classifier (400-with-context-overflow included) rather
		// than mapping the same codes a second time, and say "ollama" in the
		// prefix because this is the probe's own error, not an inherited one.
		where := fmt.Sprintf("provider: ollama probe: HTTP %d", status)
		if detail := openAIErrorDetail(body); detail != "" {
			where += ": " + detail
		}
		return "", fmt.Errorf("%w: %s", classifyOpenAIStatus(status, body), where)
	}
}

// ollamaLoading reports whether a 503 body says a model is being loaded.
//
// The rule has three parts, and each makes the label rest on evidence rather than
// on a guess:
//
//  1. The body must be this family's JSON error document ({"error": "..."} or
//     {"error": {"message": ...}}). A non-JSON body determines nothing: an HTML
//     503 page is a reverse proxy, and proxy pages routinely contain the word
//     "loading" (a spinner's id, a script's name), so matching substrings in one
//     is how a dead endpoint would be called warming forever. A plain-text
//     "Loading model" is refused for the same reason: the documented servers
//     answer JSON, and a body without the envelope is not one of them.
//
//  2. The error text must not name a failure. "loading" appears in "error loading
//     model weights" and "failed to load" appears in a load that died; those are
//     a broken server, and labelling them warming tells the operator to wait for a
//     load that will not finish. Failure language vetoes a loading phrase, because
//     a load that failed is not a load in progress. Only the message is scanned,
//     never the whole envelope: llama.cpp's own loading body carries
//     {"type":"unavailable_error"}, whose "error" would otherwise veto every real
//     warming response.
//
//  3. The text must contain one of the in-progress phrases below. They are the
//     spellings the servers in this family use for "not up yet", and each names a
//     transition rather than a state: anything that is merely unavailable without
//     saying so is down.
//
// One case is deliberately on the down side: Ollama's own 503 for a full queue
// ("server busy, please try again. maximum queue of pending requests exceeded")
// is a temporary overload, not a model load. warming is a cause — "a model is
// loading, wait" — not a severity for every temporary 503, and an overloaded
// endpoint whose body names no load is reported as what it said.
//
// The phrases are matched case-insensitively against the message text, wherever
// in the message the server puts them.
var ollamaLoadingPhrases = []string{
	"loading",   // llama.cpp "Loading model"; "model ... is loading"
	"warming",   // "the runner is warming up"
	"starting",  // "upstream model ... is starting"
	"not ready", // "model not ready yet"
}

// ollamaLoadFailurePhrases veto a loading phrase: a load that failed, could not
// start, or ran out of memory is down, however many times its message says
// "load". They are prefixes of their stems on purpose ("fail" covers failed,
// failure and failing) and are matched against the message only.
var ollamaLoadFailurePhrases = []string{
	"fail",
	"cannot",
	"can't",
	"unable",
	"error",
	"refused",
	"denied",
	"out of memory",
	"crash",
	"exited",
	"not found",
}

// ollamaLoading is consulted only for a 503 (see Probe); a 500 that says
// "Loading model" is a server that broke while loading, not a temporary state
// with a name.
func ollamaLoading(body []byte) bool {
	message, ok := ollamaErrorMessage(body)
	if !ok {
		return false
	}
	text := strings.ToLower(message)
	for _, phrase := range ollamaLoadFailurePhrases {
		if strings.Contains(text, phrase) {
			return false
		}
	}
	for _, phrase := range ollamaLoadingPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// ollamaErrorMessage extracts the message from the documented error envelope: the
// error is a string on Ollama's own routes and an object with a message on the
// OpenAI-compatible ones.
//
// It returns no message — not an empty one — when the body is not a JSON object
// with an "error" field, and when the field carries neither a non-empty string nor
// a non-empty nested message. That includes {"error":null} and {"error":""}:
// unmarshalling null into a string is a no-op that leaves it empty, so the two are
// the same absence here. Only a real message is ever returned, because the caller
// scans what it gets for phrases and a scan of nothing must not look like a scan
// of something.
func ollamaErrorMessage(body []byte) (string, bool) {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Error) == 0 {
		return "", false
	}
	var text string
	if err := json.Unmarshal(envelope.Error, &text); err == nil && text != "" {
		return text, true
	}
	var nested struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(envelope.Error, &nested); err == nil && nested.Message != "" {
		return nested.Message, true
	}
	// Neither shape produced a message. The envelope's raw bytes are deliberately
	// NOT scanned as a fallback: an error object with fields this adapter does not
	// know ({ "loading_status": true }) is not a message, and classifying it as one
	// would contradict "only the message is scanned".
	return "", false
}

// ollamaTagsModels returns the model names in a GET /api/tags body and whether the
// body is that shape at all.
//
// It reads only what "this is the model-list route" needs: an object with a
// models array, whose entries are objects naming a model. Every other field —
// size, digest, details, families, whatever a later release adds — is ignored,
// because a probe pinned to today's details would break on tomorrow's Ollama for
// no gain: Probe returns a health, not a model inventory. A body without the
// models array is not this route's answer, and the likeliest wrong one (an
// OpenAI /v1/models list) is worth reporting as an error rather than as healthy.
func ollamaTagsModels(body []byte) ([]string, bool) {
	var envelope struct {
		Models json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false
	}
	// A missing field and an explicit null both arrive as nothing here, and
	// neither is the array the route promises. Testing the first byte keeps
	// json.Unmarshal's tolerance for "null" from accepting it.
	raw := bytes.TrimSpace(envelope.Models)
	if len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, false
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		var model struct {
			Name string `json:"name"`
			// Older releases (and the field's own name in some clients) spell the
			// same fact "model"; either identifies the row.
			Model string `json:"model"`
		}
		if err := json.Unmarshal(entry, &model); err != nil {
			// An entry that is not an object is not a model row: the array came
			// from something that is not this route.
			return nil, false
		}
		name := model.Name
		if name == "" {
			name = model.Model
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return names, true
}
