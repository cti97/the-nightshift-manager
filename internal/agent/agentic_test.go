package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/board"
	"github.com/cti97/the-nightshift-manager/internal/provider"
)

// testProvider is a Provider driven by closures, because the executor's two
// interesting provider-side facts are things the scripted stub cannot expose:
// the exact request it was sent (MaxTokens, system, tool defs, tool results) and
// what EstimateTokens answered before a call. A retry test also needs a failure
// followed by a success, which a stub script cannot express.
type testProvider struct {
	name string

	mu        sync.Mutex
	calls     int
	requests  []provider.Request
	estimated []provider.Request

	complete func(call int, req provider.Request) (provider.Response, error)
	estimate func(call int, req provider.Request) int
}

func (p *testProvider) Name() string { return p.name }

func (p *testProvider) Complete(ctx context.Context, req provider.Request) (provider.Response, error) {
	p.mu.Lock()
	call := p.calls
	p.calls++
	p.requests = append(p.requests, cloneRequest(req))
	fn := p.complete
	p.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return provider.Response{}, err
	}
	if fn == nil {
		return provider.Response{}, errors.New("test provider: no scripted response")
	}
	return fn(call, req)
}

// EstimateTokens answers with the closure when one is set, and otherwise with
// the package's documented default. The call index is the index of the request
// about to be sent, so a test can make the estimate depend on what the
// conversation has grown to contain.
func (p *testProvider) EstimateTokens(req provider.Request) int {
	p.mu.Lock()
	call := p.calls
	p.estimated = append(p.estimated, cloneRequest(req))
	fn := p.estimate
	p.mu.Unlock()
	if fn != nil {
		return fn(call, req)
	}
	return provider.NewStub(nil).EstimateTokens(req)
}

func (p *testProvider) Probe(context.Context) (provider.Health, error) { return provider.HealthOK, nil }

func (p *testProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *testProvider) request(i int) provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i < 0 || i >= len(p.requests) {
		panic(fmt.Sprintf("test provider: request %d does not exist (%d recorded)", i, len(p.requests)))
	}
	return p.requests[i]
}

func (p *testProvider) estimatedRequests() []provider.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]provider.Request(nil), p.estimated...)
}

func cloneRequest(req provider.Request) provider.Request {
	out := req
	out.Messages = make([]provider.Message, len(req.Messages))
	for i, m := range req.Messages {
		c := m
		c.ToolCalls = make([]provider.Call, len(m.ToolCalls))
		for j, call := range m.ToolCalls {
			call.Arguments = bytes.Clone(call.Arguments)
			c.ToolCalls[j] = call
		}
		out.Messages[i] = c
	}
	out.Tools = make([]provider.ToolDef, len(req.Tools))
	for i, tool := range req.Tools {
		tool.Schema = bytes.Clone(tool.Schema)
		out.Tools[i] = tool
	}
	return out
}

// --- scripted responses ----------------------------------------------------

func callResponse(id, name, args string, usage provider.Usage) provider.Response {
	return provider.Response{
		ToolCalls: []provider.Call{{ID: id, Name: name, Arguments: json.RawMessage(args)}},
		Usage:     usage,
	}
}

func finishResponse(id, summary string, usage provider.Usage) provider.Response {
	args, err := json.Marshal(map[string]string{"summary": summary})
	if err != nil {
		panic(err)
	}
	return callResponse(id, ToolFinish, string(args), usage)
}

func listDirResponse(id string) provider.Response {
	return callResponse(id, ToolListDir, `{}`, provider.Usage{In: 10, Out: 5})
}

func finishUsage() provider.Usage { return provider.Usage{In: 10, Out: 5} }

func claimedItem(runID, brief string, needs ...string) board.ClaimedItem {
	return board.ClaimedItem{
		Item:  board.WorkItem{ID: "item-1", Title: "a task", Needs: needs},
		Brief: brief,
		RunID: runID,
	}
}

// lastMessageOfRole walks backwards for the newest turn of a role. It is how the
// tests read what the model was actually told, which is the observable that
// matters for tool results and nudges.
func lastMessageOfRole(req provider.Request, role string) (provider.Message, bool) {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == role {
			return req.Messages[i], true
		}
	}
	return provider.Message{}, false
}

func newAgentic(t *testing.T, p provider.Provider) *AgenticExecutor {
	t.Helper()
	return &AgenticExecutor{
		Provider:    p,
		Model:       "test-model",
		SandboxRoot: t.TempDir(),
	}
}

// --- the loop --------------------------------------------------------------

// TestAgenticExecutorRunsToFinish is the smallest true story: one provider
// call, one finish call, the summary and the usage survive to the Result.
func TestAgenticExecutorRunsToFinish(t *testing.T) {
	stub := provider.NewStub([]provider.Response{finishResponse("call-finish", "finished: do it", finishUsage())})
	exec := newAgentic(t, stub)

	res, err := exec.Run(context.Background(), claimedItem("run-1", "do it", "agentic"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "finished: do it" {
		t.Fatalf("Summary = %q, want the finish summary", res.Summary)
	}
	if res.TokensIn != 10 || res.TokensOut != 5 {
		t.Fatalf("tokens = (%d, %d), want (10, 5)", res.TokensIn, res.TokensOut)
	}
}

// TestAgenticExecutorSetsMaxTokensModelAndToolsOnEveryCall is Task 5's binding
// hand-off: the Anthropic adapter refuses a zero cap locally, so every request
// must carry MaxTokens or an anthropic endpoint fails its first step. The tool
// set and model are asserted here too because they are the same request shape.
func TestAgenticExecutorSetsMaxTokensModelAndToolsOnEveryCall(t *testing.T) {
	p := &testProvider{name: "acme", complete: func(call int, _ provider.Request) (provider.Response, error) {
		if call == 0 {
			return listDirResponse("call-list"), nil
		}
		return finishResponse("call-finish", "done", finishUsage()), nil
	}}
	exec := newAgentic(t, p)
	exec.Model = "model-7"
	exec.MaxTokens = 77

	if _, err := exec.Run(context.Background(), claimedItem("run-1", "do it", "agentic")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := p.callCount(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	for i := 0; i < p.callCount(); i++ {
		req := p.request(i)
		if req.MaxTokens != 77 {
			t.Fatalf("request %d MaxTokens = %d, want 77", i, req.MaxTokens)
		}
		if req.Model != "model-7" {
			t.Fatalf("request %d Model = %q, want model-7", i, req.Model)
		}
		if strings.TrimSpace(req.System) == "" {
			t.Fatalf("request %d has no system prompt", i)
		}
		if len(req.Tools) != 6 {
			t.Fatalf("request %d carries %d tool definitions, want 6", i, len(req.Tools))
		}
		names := map[string]bool{}
		for _, tool := range req.Tools {
			names[tool.Name] = true
		}
		for _, want := range []string{ToolReadFile, ToolWriteFile, ToolListDir, ToolShell, ToolHTTPGet, ToolFinish} {
			if !names[want] {
				t.Fatalf("request %d tool defs are missing %q", i, want)
			}
		}
	}
}

// TestAgenticExecutorDefaults pins the two caps' absent-config values. 40 steps
// is the design's number; the token and output budgets must be positive safety
// numbers rather than a silently unlimited run.
func TestAgenticExecutorDefaults(t *testing.T) {
	e := &AgenticExecutor{}
	if got := e.stepLimit(); got != 40 {
		t.Fatalf("default step limit = %d, want 40", got)
	}
	if got := e.maxTokens(); got <= 0 {
		t.Fatalf("default MaxTokens = %d, want a positive value", got)
	}
	if got := e.tokenLimit(); got <= 0 {
		t.Fatalf("default token budget = %d, want a positive value", got)
	}
}

// TestAgenticExecutorStopsAtTheStepCap pins that the first cap names itself in
// both the error and the Result, and that it is the *counted* number of provider
// calls, not a guess.
func TestAgenticExecutorStopsAtTheStepCap(t *testing.T) {
	p := &testProvider{name: "acme", complete: func(call int, _ provider.Request) (provider.Response, error) {
		return listDirResponse(fmt.Sprintf("call-%d", call)), nil
	}}
	exec := newAgentic(t, p)
	exec.MaxSteps = 3

	res, err := exec.Run(context.Background(), claimedItem("run-1", "loop forever", "agentic"))
	if err == nil {
		t.Fatal("Run succeeded; a run that never called finish must not report success")
	}
	if !errors.Is(err, ErrStepCap) {
		t.Fatalf("Run error = %v, want ErrStepCap", err)
	}
	if !strings.Contains(err.Error(), "step cap") {
		t.Fatalf("error %q does not name the step cap", err)
	}
	if !strings.Contains(err.Error(), "3") {
		t.Fatalf("error %q does not name the step limit", err)
	}
	if got := p.callCount(); got != 3 {
		t.Fatalf("provider calls = %d, want exactly 3 (the cap)", got)
	}
	if !strings.Contains(res.Summary, "step cap") {
		t.Fatalf("Result.Summary = %q, want the cap named", res.Summary)
	}
}

// TestAgenticExecutorStopsAtTheTokenBudget pins the second cap: no provider
// call is even attempted when the reservation cannot fit.
func TestAgenticExecutorStopsAtTheTokenBudget(t *testing.T) {
	p := &testProvider{name: "acme", complete: func(call int, _ provider.Request) (provider.Response, error) {
		return finishResponse("call-finish", "done", finishUsage()), nil
	}}
	exec := newAgentic(t, p)
	exec.TokenBudget = 10

	res, err := exec.Run(context.Background(), claimedItem("run-1", "do it", "agentic"))
	if err == nil {
		t.Fatal("Run succeeded; a request that cannot fit the budget must not be sent")
	}
	if !errors.Is(err, ErrTokenBudget) {
		t.Fatalf("Run error = %v, want ErrTokenBudget", err)
	}
	if !strings.Contains(err.Error(), "token budget") {
		t.Fatalf("error %q does not name the token budget", err)
	}
	if got := p.callCount(); got != 0 {
		t.Fatalf("provider calls = %d, want 0: the budget was already exceeded", got)
	}
	if !strings.Contains(res.Summary, "token budget") {
		t.Fatalf("Result.Summary = %q, want the cap named", res.Summary)
	}
}

// TestAgenticExecutorCountsToolOutputBeforeTheNextRequest is the hand-off that
// matters most for the bill: a 4 MiB read_file is roughly a million tokens, so
// the grown conversation is estimated and refused BEFORE the next request is
// sent, not discovered afterwards.
func TestAgenticExecutorCountsToolOutputBeforeTheNextRequest(t *testing.T) {
	p := &testProvider{name: "acme", complete: func(call int, _ provider.Request) (provider.Response, error) {
		if call == 0 {
			return listDirResponse("call-list"), nil
		}
		return finishResponse("call-finish", "done", finishUsage()), nil
	}}
	// The estimate only grows once a tool result is in the conversation, which
	// is exactly the growth the executor must account for. 50k tokens is chosen
	// to be unmissable against the 2k budget.
	p.estimate = func(_ int, req provider.Request) int {
		for _, m := range req.Messages {
			if m.Role == "tool" {
				return 50_000
			}
		}
		return 10
	}
	exec := newAgentic(t, p)
	exec.TokenBudget = 2_000
	exec.MaxTokens = 100

	res, err := exec.Run(context.Background(), claimedItem("run-1", "read a lot", "agentic"))
	if err == nil {
		t.Fatal("Run succeeded; the grown conversation did not fit the budget")
	}
	if !errors.Is(err, ErrTokenBudget) {
		t.Fatalf("Run error = %v, want ErrTokenBudget", err)
	}
	if got := p.callCount(); got != 1 {
		t.Fatalf("provider calls = %d, want 1: the second request must not be sent", got)
	}
	if !strings.Contains(res.Summary, "token budget") {
		t.Fatalf("Result.Summary = %q, want the cap named", res.Summary)
	}
	est := p.estimatedRequests()
	if len(est) != 2 {
		t.Fatalf("estimate calls = %d, want 2 (before the first request and before the refused second)", len(est))
	}
	tool, ok := lastMessageOfRole(est[1], "tool")
	if !ok {
		t.Fatalf("the second estimate saw no tool result; the executor did not count it: %+v", est[1].Messages)
	}
	if !strings.Contains(tool.Content, "tmp/") {
		t.Fatalf("second estimate tool result = %q, want the list_dir output", tool.Content)
	}
}

// TestAgenticExecutorNeverReportsSuccessWithoutFinish pins the property the
// caps exist for: no finish call means no success, however many chatty turns a
// model produces.
func TestAgenticExecutorNeverReportsSuccessWithoutFinish(t *testing.T) {
	p := &testProvider{name: "acme", complete: func(call int, _ provider.Request) (provider.Response, error) {
		// Stop deliberately says max_tokens: the seam documents no vocabulary
		// for it and Task 5's note says not to branch on it, so a truncated
		// reply is still a reply that did not call finish.
		return provider.Response{Content: "I am thinking about it.", Stop: "max_tokens", Usage: finishUsage()}, nil
	}}
	exec := newAgentic(t, p)
	exec.MaxSteps = 2

	res, err := exec.Run(context.Background(), claimedItem("run-1", "do it", "agentic"))
	if err == nil {
		t.Fatalf("Run succeeded without a finish call; summary = %q", res.Summary)
	}
	if !errors.Is(err, ErrStepCap) {
		t.Fatalf("Run error = %v, want ErrStepCap", err)
	}
	if got := p.callCount(); got != 2 {
		t.Fatalf("provider calls = %d, want 2", got)
	}
	// The second request must carry the nudge that tells the model to call a
	// tool, so a chatty model is not just re-sent the same prompt.
	last, ok := lastMessageOfRole(p.request(1), "user")
	if !ok || !strings.Contains(last.Content, "tool") {
		t.Fatalf("second request's last user turn = %q, want a tool nudge", last.Content)
	}
}

// --- the board-facing cap test --------------------------------------------

// TestAgenticExecutorReportsACapTripAsAnErrorNotASuccess is the acceptance test
// that must fail on a truncated success: the item is `failed` with a reason, not
// `done` with an empty summary.
func TestAgenticExecutorReportsACapTripAsAnErrorNotASuccess(t *testing.T) {
	f := newFixture(t)

	stub := provider.NewStub([]provider.Response{
		listDirResponse("call-1"),
		listDirResponse("call-2"),
	})
	exec := newAgentic(t, stub)
	exec.MaxSteps = 2

	a := New(f.config(exec))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errc := startAgent(a, ctx)

	waitFor(t, "the item failed on the step cap", func() bool { return f.itemStatus(t) == "failed" })
	cancel()
	waitAgent(t, errc, context.Canceled)

	if got := f.runStatus(t); got != "error" {
		t.Fatalf("run status = %q, want error", got)
	}
	var runErr string
	if err := f.db.QueryRow(
		`SELECT COALESCE(error,'') FROM runs WHERE work_item_id=? ORDER BY attempt DESC LIMIT 1`,
		f.itemID).Scan(&runErr); err != nil {
		t.Fatalf("read run: %v", err)
	}
	var summary string
	if err := f.db.QueryRow(`SELECT COALESCE(result_summary,'') FROM work_items WHERE id=?`, f.itemID).
		Scan(&summary); err != nil {
		t.Fatalf("read item summary: %v", err)
	}
	if !strings.Contains(runErr, "step cap") {
		t.Fatalf("run error = %q, want the step cap named", runErr)
	}
	if !strings.Contains(summary, "step cap") {
		t.Fatalf("result_summary = %q, want the step cap named", summary)
	}
}

// --- provider taxonomy -----------------------------------------------------

// TestAgenticExecutorRoutesThroughTheProvidersErrorTaxonomy pins the three
// verdicts the executor must honour: rate_limited is retried, auth fails once
// with the provider named, and a placement bug is surfaced as a bug report and
// never retried.
func TestAgenticExecutorRoutesThroughTheProvidersErrorTaxonomy(t *testing.T) {
	t.Run("rate_limited retries", func(t *testing.T) {
		p := &testProvider{name: "acme", complete: func(call int, _ provider.Request) (provider.Response, error) {
			if call == 0 {
				return provider.Response{}, provider.ErrRateLimited
			}
			return finishResponse("call-finish", "made it", finishUsage()), nil
		}}
		exec := newAgentic(t, p)
		var slept []time.Duration
		exec.sleep = func(ctx context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		}

		res, err := exec.Run(context.Background(), claimedItem("run-1", "do it", "agentic"))
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Summary != "made it" {
			t.Fatalf("Summary = %q, want the retried finish summary", res.Summary)
		}
		if got := p.callCount(); got != 2 {
			t.Fatalf("provider calls = %d, want 2 (the failed attempt and the retry)", got)
		}
		if len(slept) != 1 || slept[0] != provider.Backoff(1) {
			t.Fatalf("sleeps = %v, want one of %s", slept, provider.Backoff(1))
		}
	})

	t.Run("auth fails once with the provider named", func(t *testing.T) {
		p := &testProvider{name: "acme", complete: func(int, provider.Request) (provider.Response, error) {
			return provider.Response{}, provider.ErrAuth
		}}
		exec := newAgentic(t, p)
		var slept []time.Duration
		exec.sleep = func(ctx context.Context, d time.Duration) error {
			slept = append(slept, d)
			return nil
		}

		_, err := exec.Run(context.Background(), claimedItem("run-1", "do it", "agentic"))
		if err == nil {
			t.Fatal("Run succeeded; an auth failure must fail the run")
		}
		if !errors.Is(err, provider.ErrAuth) {
			t.Fatalf("Run error = %v, want the provider's ErrAuth", err)
		}
		if got := p.callCount(); got != 1 {
			t.Fatalf("provider calls = %d, want 1: a 401 must not be retried", got)
		}
		if len(slept) != 0 {
			t.Fatalf("slept %v, want no wait before a non-retryable failure", slept)
		}
		msg := err.Error()
		if !strings.Contains(msg, "acme") {
			t.Fatalf("error %q does not name the provider", msg)
		}
		if !strings.Contains(msg, "auth") {
			t.Fatalf("error %q does not name the error class", msg)
		}
		// decision.Reason is the operator-facing half of the decision; it must
		// be attached, not replaced by a bare "provider failed".
		if !strings.Contains(msg, "credential") {
			t.Fatalf("error %q does not carry the retry decision's reason", msg)
		}
	})

	t.Run("a placement bug is not retried and says so", func(t *testing.T) {
		p := &testProvider{name: "acme", complete: func(int, provider.Request) (provider.Response, error) {
			return provider.Response{}, provider.ErrContextOverflow
		}}
		exec := newAgentic(t, p)

		_, err := exec.Run(context.Background(), claimedItem("run-1", "do it", "agentic"))
		if err == nil {
			t.Fatal("Run succeeded; a context overflow must fail the run")
		}
		if !errors.Is(err, provider.ErrContextOverflow) {
			t.Fatalf("Run error = %v, want the provider's ErrContextOverflow", err)
		}
		if got := p.callCount(); got != 1 {
			t.Fatalf("provider calls = %d, want 1: a placement bug must not be retried", got)
		}
		msg := err.Error()
		if !strings.Contains(msg, "placement bug") {
			t.Fatalf("error %q does not call the context overflow what it is: a placement bug", msg)
		}
		if !strings.Contains(msg, "placement computed the token fit wrongly") {
			t.Fatalf("error %q does not carry decision.Reason", msg)
		}
	})
}

// --- tool dispatch ---------------------------------------------------------

// TestAgenticExecutorFeedsToolErrorsBackToTheModel pins the tool layer's
// contract: on any failure other than finish, the model gets the output and the
// error together, attributed to the call it answers.
func TestAgenticExecutorFeedsToolErrorsBackToTheModel(t *testing.T) {
	p := &testProvider{name: "acme", complete: func(call int, _ provider.Request) (provider.Response, error) {
		if call == 0 {
			return callResponse("call-read", ToolReadFile, `{"path":"missing.txt"}`, provider.Usage{In: 10, Out: 5}), nil
		}
		return finishResponse("call-finish", "done", finishUsage()), nil
	}}
	exec := newAgentic(t, p)

	if _, err := exec.Run(context.Background(), claimedItem("run-1", "read it", "agentic")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tool, ok := lastMessageOfRole(p.request(1), "tool")
	if !ok {
		t.Fatalf("second request carries no tool result: %+v", p.request(1).Messages)
	}
	if tool.CallID != "call-read" {
		t.Fatalf("tool result CallID = %q, want call-read", tool.CallID)
	}
	if !strings.Contains(tool.Content, "missing.txt") {
		t.Fatalf("tool result %q does not carry the tool's error", tool.Content)
	}
	if !strings.Contains(tool.Content, "sandbox: read") {
		t.Fatalf("tool result %q does not carry the sandbox's own text", tool.Content)
	}
}

// TestAgenticExecutorAnswersAnUnknownToolWithTheClosedSet pins the closed-set
// rule: a hallucinated tool is a step the model can correct, not a failed run.
func TestAgenticExecutorAnswersAnUnknownToolWithTheClosedSet(t *testing.T) {
	p := &testProvider{name: "acme", complete: func(call int, _ provider.Request) (provider.Response, error) {
		if call == 0 {
			return callResponse("call-bogus", "rm_rf", `{}`, provider.Usage{In: 10, Out: 5}), nil
		}
		return finishResponse("call-finish", "recovered", finishUsage()), nil
	}}
	exec := newAgentic(t, p)

	res, err := exec.Run(context.Background(), claimedItem("run-1", "do it", "agentic"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "recovered" {
		t.Fatalf("Summary = %q, want the run to continue after the unknown tool", res.Summary)
	}
	tool, ok := lastMessageOfRole(p.request(1), "tool")
	if !ok {
		t.Fatalf("second request carries no tool result: %+v", p.request(1).Messages)
	}
	if !strings.Contains(tool.Content, "unknown tool") {
		t.Fatalf("tool result = %q, want it to name the unknown tool", tool.Content)
	}
	for _, want := range []string{ToolReadFile, ToolFinish} {
		if !strings.Contains(tool.Content, want) {
			t.Fatalf("tool result %q does not list %q", tool.Content, want)
		}
	}
}

// TestAgenticExecutorTruncatesAnOversizedToolOutput pins the per-call cap that
// makes hand-off 2 enforceable in practice: the tool layer's 4 MiB read is
// larger than any context, so the executor refuses to put it in the
// conversation and tells the model how to proceed instead.
func TestAgenticExecutorTruncatesAnOversizedToolOutput(t *testing.T) {
	const size = 70_000
	writeArgs, err := json.Marshal(map[string]string{"path": "big.txt", "content": strings.Repeat("a", size)})
	if err != nil {
		t.Fatalf("marshal write args: %v", err)
	}
	p := &testProvider{name: "acme", complete: func(call int, _ provider.Request) (provider.Response, error) {
		switch call {
		case 0:
			return callResponse("call-write", ToolWriteFile, string(writeArgs), provider.Usage{In: 10, Out: 5}), nil
		case 1:
			return callResponse("call-read", ToolReadFile, `{"path":"big.txt"}`, provider.Usage{In: 10, Out: 5}), nil
		default:
			return finishResponse("call-finish", "done", finishUsage()), nil
		}
	}}
	exec := newAgentic(t, p)

	if _, err := exec.Run(context.Background(), claimedItem("run-1", "read a big file", "agentic")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	tool, ok := lastMessageOfRole(p.request(2), "tool")
	if !ok {
		t.Fatalf("third request carries no tool result: %+v", p.request(2).Messages)
	}
	if len(tool.Content) >= size {
		t.Fatalf("tool result is %d bytes; the %d-byte output was not capped", len(tool.Content), size)
	}
	if !strings.Contains(tool.Content, "cap") {
		t.Fatalf("tool result = %q, want it to name the tool-output cap", tool.Content)
	}
	if !strings.Contains(tool.Content, "parts") {
		t.Fatalf("tool result = %q, want it to tell the model how to proceed", tool.Content)
	}
}

// --- capability plumbing ---------------------------------------------------

// TestAgenticExecutorGatesHTTPGetOnTheItemsNeed pins hand-off 1: the sandbox's
// network flag comes from the item's declared needs, so an item without
// "network" cannot reach the wire even through http_get.
func TestAgenticExecutorGatesHTTPGetOnTheItemsNeed(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "hello from the server")
	}))
	defer srv.Close()

	args := fmt.Sprintf(`{"url":%q}`, srv.URL)
	script := func(p *testProvider) {
		p.complete = func(call int, _ provider.Request) (provider.Response, error) {
			if call == 0 {
				return callResponse("call-get", ToolHTTPGet, args, provider.Usage{In: 10, Out: 5}), nil
			}
			return finishResponse("call-finish", "done", finishUsage()), nil
		}
	}

	t.Run("without the network need the request is refused", func(t *testing.T) {
		p := &testProvider{name: "acme"}
		script(p)
		exec := newAgentic(t, p)
		if _, err := exec.Run(context.Background(), claimedItem("run-1", "fetch", "agentic")); err != nil {
			t.Fatalf("Run: %v", err)
		}
		tool, ok := lastMessageOfRole(p.request(1), "tool")
		if !ok {
			t.Fatalf("second request carries no tool result: %+v", p.request(1).Messages)
		}
		if !strings.Contains(tool.Content, "refused") || !strings.Contains(tool.Content, "network") {
			t.Fatalf("tool result = %q, want the capability refusal", tool.Content)
		}
		if got := hits.Load(); got != 0 {
			t.Fatalf("server was hit %d times; the tool sent a request without the network need", got)
		}
	})

	t.Run("with the network need the request is sent", func(t *testing.T) {
		p := &testProvider{name: "acme"}
		script(p)
		exec := newAgentic(t, p)
		if _, err := exec.Run(context.Background(), claimedItem("run-2", "fetch", "agentic", "network")); err != nil {
			t.Fatalf("Run: %v", err)
		}
		tool, ok := lastMessageOfRole(p.request(1), "tool")
		if !ok {
			t.Fatalf("second request carries no tool result: %+v", p.request(1).Messages)
		}
		if !strings.Contains(tool.Content, "hello from the server") {
			t.Fatalf("tool result = %q, want the fetched body", tool.Content)
		}
		if got := hits.Load(); got != 1 {
			t.Fatalf("server was hit %d times, want 1", got)
		}
	})
}

// TestAgenticExecutorRequiresLocalForANoEgressItem pins the provider-layer half
// of no_egress: the executor must tell the adapter what the item declared, or
// the adapter's last-line check cannot run.
func TestAgenticExecutorRequiresLocalForANoEgressItem(t *testing.T) {
	for _, tc := range []struct {
		name  string
		needs []string
		want  bool
	}{
		{name: "no_egress", needs: []string{"agentic", "no_egress"}, want: true},
		{name: "strict_local", needs: []string{"agentic", "strict_local"}, want: true},
		{name: "ordinary item", needs: []string{"agentic"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &testProvider{name: "acme", complete: func(int, provider.Request) (provider.Response, error) {
				return finishResponse("call-finish", "done", finishUsage()), nil
			}}
			exec := newAgentic(t, p)
			if _, err := exec.Run(context.Background(), claimedItem("run-1", "do it", tc.needs...)); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := p.request(0).RequireLocal; got != tc.want {
				t.Fatalf("request RequireLocal = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- lifecycle -------------------------------------------------------------

// TestAgenticExecutorClosesTheSandboxOnEveryPath pins hand-off 3 on the paths a
// test can reach: a successful run, a provider failure on the first call, and a
// cap trip. A leaked run directory is invisible until the disk fills.
func TestAgenticExecutorClosesTheSandboxOnEveryPath(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		root := t.TempDir()
		stub := provider.NewStub([]provider.Response{finishResponse("call-finish", "done", finishUsage())})
		exec := &AgenticExecutor{Provider: stub, Model: "m", SandboxRoot: root}
		if _, err := exec.Run(context.Background(), claimedItem("run-1", "do it", "agentic")); err != nil {
			t.Fatalf("Run: %v", err)
		}
		assertRunDirGone(t, root, "run-1")
	})

	t.Run("provider fails on the first call", func(t *testing.T) {
		root := t.TempDir()
		p := &testProvider{name: "acme", complete: func(int, provider.Request) (provider.Response, error) {
			return provider.Response{}, provider.ErrAuth
		}}
		exec := &AgenticExecutor{Provider: p, Model: "m", SandboxRoot: root}
		if _, err := exec.Run(context.Background(), claimedItem("run-1", "do it", "agentic")); err == nil {
			t.Fatal("Run succeeded; the provider refused")
		}
		assertRunDirGone(t, root, "run-1")
	})

	t.Run("step cap", func(t *testing.T) {
		root := t.TempDir()
		stub := provider.NewStub([]provider.Response{listDirResponse("call-1")})
		exec := &AgenticExecutor{Provider: stub, Model: "m", SandboxRoot: root, MaxSteps: 1}
		if _, err := exec.Run(context.Background(), claimedItem("run-1", "loop", "agentic")); err == nil {
			t.Fatal("Run succeeded; the step cap should have ended it")
		}
		assertRunDirGone(t, root, "run-1")
	})
}

func assertRunDirGone(t *testing.T, root, runID string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(root, runID))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run directory %q still exists (stat err = %v); Close was not reached", filepath.Join(root, runID), err)
	}
}

// TestAgenticExecutorLogsKernelConfinementOnce pins hand-off 3's other half: the
// operator is told once whether the kernel layer is holding, and the operator is
// never told it is when it is not.
func TestAgenticExecutorLogsKernelConfinementOnce(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	p := &testProvider{name: "acme", complete: func(int, provider.Request) (provider.Response, error) {
		return finishResponse("call-finish", "done", finishUsage()), nil
	}}
	exec := newAgentic(t, p)
	exec.logFn = func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}

	for _, runID := range []string{"run-1", "run-2"} {
		if _, err := exec.Run(context.Background(), claimedItem(runID, "do it", "agentic")); err != nil {
			t.Fatalf("Run %s: %v", runID, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	kernelLines := 0
	for _, line := range lines {
		if strings.Contains(strings.ToLower(line), "kernel confinement") {
			kernelLines++
		}
	}
	if kernelLines != 1 {
		t.Fatalf("kernel confinement logged %d times, want exactly 1; lines = %v", kernelLines, lines)
	}
}

// TestAgenticExecutorReturnsTheContextErrorWhenCancelledMidTool pins the
// Executor contract: Run returns promptly with the context's error, so the
// agent's loop skips the report and leaves the run to the reaper.
func TestAgenticExecutorReturnsTheContextErrorWhenCancelledMidTool(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-r.Context().Done()
	}))
	defer srv.Close()

	args := fmt.Sprintf(`{"url":%q}`, srv.URL)
	p := &testProvider{name: "acme", complete: func(call int, _ provider.Request) (provider.Response, error) {
		if call == 0 {
			return callResponse("call-get", ToolHTTPGet, args, provider.Usage{In: 10, Out: 5}), nil
		}
		return finishResponse("call-finish", "done", finishUsage()), nil
	}}
	exec := newAgentic(t, p)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := exec.Run(ctx, claimedItem("run-1", "fetch", "agentic", "network"))
		errc <- err
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("http_get never reached the server")
	}
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return promptly after cancellation")
	}
	if got := p.callCount(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
}

// --- M1's end-to-end test, re-run -----------------------------------------

// TestAgentRunsAnAgenticExecutorToDone re-runs M1's queued → done story with
// AgenticExecutor and the stub provider in place of StubExecutor. It is the
// milestone's proof that the real loop reports through the same Client as the
// stub: the board settles the item, records the finish summary, and stores the
// usage the provider reported.
func TestAgentRunsAnAgenticExecutorToDone(t *testing.T) {
	f := newFixture(t)

	stub := provider.NewStub([]provider.Response{
		finishResponse("call-finish", "agentic did it", provider.Usage{In: 12, Out: 7}),
	})
	exec := newAgentic(t, stub)

	a := New(f.config(exec))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errc := startAgent(a, ctx)

	waitFor(t, "item done", func() bool { return f.itemStatus(t) == "done" })
	cancel()
	waitAgent(t, errc, context.Canceled)

	var status, summary string
	if err := f.db.QueryRow(`SELECT status, result_summary FROM work_items WHERE id=?`, f.itemID).
		Scan(&status, &summary); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if status != "done" {
		t.Fatalf("item status = %q, want done", status)
	}
	if summary != "agentic did it" {
		t.Fatalf("result_summary = %q, want the finish summary", summary)
	}
	if got := f.runStatus(t); got != "ok" {
		t.Fatalf("run status = %q, want ok", got)
	}

	var tokensIn, tokensOut int
	if err := f.db.QueryRow(
		`SELECT input_tokens, output_tokens FROM runs WHERE work_item_id=? ORDER BY attempt DESC LIMIT 1`,
		f.itemID).Scan(&tokensIn, &tokensOut); err != nil {
		t.Fatalf("read run tokens: %v", err)
	}
	if tokensIn != 12 || tokensOut != 7 {
		t.Fatalf("run tokens = (%d, %d), want (12, 7)", tokensIn, tokensOut)
	}
}
