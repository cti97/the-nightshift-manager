package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/board"
	"github.com/cti97/the-nightshift-manager/internal/provider"
)

// This file is the agentic executor: the loop that actually runs a task
// (design §10). It builds a conversation from the claim's brief, asks the
// configured provider for the next turn, dispatches the tool calls the model
// emits, and stops on `finish` or on one of two caps. It reports through the
// same Executor return path as StubExecutor, so an item that hits a cap is
// reported as a failure naming the cap — never as a truncated success.
//
// # The two caps, and why both
//
// A step cap alone lets one enormous step spend an unbounded number of tokens;
// a token budget alone lets a model make thousands of cheap, useless calls.
// Whichever trips first ends the run, and the error and the Result.Summary both
// name which one did. A run that silently stops at 40 steps and reports "done"
// is indistinguishable from a run that finished, which is the failure this
// file exists to prevent.
//
// # What the executor does not own
//
// It does not *choose* the endpoint or the model: placement did, and the claim
// payload carries that choice (board.ClaimedItem.Endpoint/Model). Run builds the
// adapter from it, resolving the credential by name from this node's own
// environment — the value never transits the board. It does not report: Run
// returns, and the agent loop's Client.Report retries the result. It does not
// read a clock: every wait is a timer on the run's context, and there is
// nothing in this file whose correctness depends on wall time.

// The caps' defaults. maxSteps is design §10's "~40". The token budget has no
// number in the design, so 200k is this implementation's conservative ceiling:
// a 40-step run that re-sends a few thousand tokens of conversation per step
// fits comfortably, while a runaway conversation does not. MaxTokens is the
// per-call output cap, and it must be positive on every request: the Anthropic
// adapter refuses a zero cap locally (Task 5), and the output cap is the
// caller's spending decision, so this executor makes it explicitly.
const (
	defaultMaxSteps    = 40
	defaultMaxTokens   = 1024
	defaultTokenBudget = 200_000

	// maxToolOutputBytes is the per-call cap on what a tool result may put into
	// the conversation. The tool layer's own limits are larger (a read_file can
	// return 4 MiB), which is roughly a million tokens: large enough to spend a
	// whole context window on text the model cannot use. The executor therefore
	// refuses to append the output and tells the model so, instead of sending
	// it (Task 11's hand-off 2).
	maxToolOutputBytes = 64 << 10
)

// ErrStepCap and ErrTokenBudget are the two ways a run can end without a finish
// call. They are sentinels so a caller can tell a cap from a provider failure
// with errors.Is; the wrapped text always names the cap and its number, because
// the operator reads the run's error, not this file.
var (
	ErrStepCap     = errors.New("agent: the step cap ended the run")
	ErrTokenBudget = errors.New("agent: the token budget ended the run")
)

// defaultSystemPrompt is the standing instruction. It deliberately repeats the
// finish contract: finish is the only way a run succeeds, and the summary is the
// only thing the operator reads.
const defaultSystemPrompt = `You are a worker executing one task on a board. You have a disposable run directory as your working directory and a closed set of tools: read_file, write_file, list_dir, shell, http_get and finish.

Work in small, verifiable steps. Use list_dir and read_file to understand what is already there, write artifacts inside the run directory, and use shell for commands. http_get is refused unless this task was placed with the network capability.

When the work is complete — or when you are certain it cannot be completed — call finish with a summary. The summary is the only thing the operator will read about this run: say what you did, where the artifacts are inside the run directory, what you verified, and what remains unfinished or uncertain. Do not call finish to ask a question.`

const noToolCallNudge = "You did not call a tool. Continue the task with one of the tools, and call finish when the work is complete or you are certain it cannot be completed."

// AgenticExecutor is an Executor that runs a real model through the tool loop.
//
// The zero value is not usable: Registry (or the test constructor) and
// SandboxRoot are required, and the endpoint/model arrive on each claim. The
// caps have defaults (see the constants above) and a non-positive field selects
// its default rather than disabling the cap — a safety number that can be
// switched off by leaving a field zero is not a safety number.
type AgenticExecutor struct {
	// Registry constructs the adapter for the endpoint the claim names. It is
	// required in production; newProvider is the test seam. Construction is per
	// run rather than per process because the endpoint is per claim, and
	// placement may legitimately choose a different one for the next item.
	Registry *provider.Registry
	// System overrides the default system prompt. It is prose configuration,
	// not a capability: the tool set is fixed by Tools().
	System string

	// MaxTokens caps one reply. Zero selects defaultMaxTokens; a zero sent to
	// the Anthropic adapter would be refused.
	MaxTokens int
	// MaxSteps caps the number of provider calls in one run. Zero selects
	// defaultMaxSteps.
	MaxSteps int
	// TokenBudget caps the tokens one run may spend or reserve. Zero selects
	// defaultTokenBudget.
	TokenBudget int

	// SandboxRoot is where this run's disposable directory is created (spec
	// §10's run_root). Required.
	SandboxRoot string
	// KeepArtifacts leaves the run directory in place on Close, for inspection.
	KeepArtifacts bool

	// newProvider is the provider-construction seam. It defaults to
	// Registry.New; a test supplies a scripted adapter without teaching the
	// registry about test doubles.
	newProvider func(provider.Config) (provider.Provider, error)
	// lookupEnv resolves an endpoint's auth_ref from this node's environment.
	// It defaults to os.Getenv and exists as a field so a test can prove the
	// name is resolved locally and that a refused name never reaches it.
	lookupEnv func(string) string
	// sleep is the retry policy's wait. It is a field rather than a call to
	// time.Sleep so a test can drive a retry without waiting, and so the wait is
	// always a timer on the run's context.
	sleep provider.SleepFunc
	// logFn is the log sink. It is a field so a test can assert the once-only
	// kernel-confinement line.
	logFn func(format string, args ...any)
	// kernelOnce makes the kernel-confinement report once per executor, not once
	// per run: an operator reading a node's log sees it once, and a run does not
	// repeat a machine-wide fact forty times across a shift.
	kernelOnce sync.Once
}

// Run implements Executor. It is the whole loop: one sandbox, one conversation,
// one provider call per step, one tool dispatch per call.
//
// The run context is passed unchanged to the retry policy and to every tool;
// the executor never creates a per-attempt timeout. That is deliberate: the
// deadline rule in provider.DecideRetry is only correct when it can tell the
// adapter's per-request timeout (retryable) from the run's own deadline (not),
// and a WithTimeout around one call would make every ordinary timeout look like
// a shutdown (Task 6's hand-off).
func (e *AgenticExecutor) Run(ctx context.Context, item board.ClaimedItem) (Result, error) {
	if err := e.check(item); err != nil {
		return Result{}, err
	}
	p, model, err := e.providerFor(item)
	if err != nil {
		return Result{}, err
	}
	e.logKernelOnce()

	meter := &tokenMeter{limit: e.tokenLimit()}

	tools := Tools()
	defs, err := Definitions(tools)
	if err != nil {
		return meter.result(""), fmt.Errorf("agent: tool definitions: %w", err)
	}

	// The network capability is the item's declaration, never a guess by this
	// executor: `Network: true` unconditionally would un-gate http_get, which is
	// the only egress rule the tool layer enforces (Task 11's hand-off 1).
	sb, err := NewSandbox(SandboxConfig{
		Root:          e.SandboxRoot,
		RunID:         item.RunID,
		Network:       slices.Contains(item.Item.Needs, "network"),
		KeepArtifacts: e.KeepArtifacts,
	})
	if err != nil {
		return meter.result(""), err
	}
	// Every path out of Run goes through this defer, including a provider
	// failure on the first call. The error is logged rather than discarded:
	// a deferred Close silently drops a refused removal, and a leaked run
	// directory is invisible until the disk fills (Task 10/11's hand-off 3).
	defer func() {
		if cerr := sb.Close(); cerr != nil {
			e.logf("run %s: sandbox close: %v", item.RunID, cerr)
		}
	}()

	messages := []provider.Message{{Role: "user", Content: userBrief(item)}}

	for step := 0; ; step++ {
		if err := ctx.Err(); err != nil {
			return meter.result(""), err
		}
		if step >= e.stepLimit() {
			summary := fmt.Sprintf("run stopped by the step cap after %d steps (max %d) without a finish call", step, e.stepLimit())
			return meter.result(summary),
				fmt.Errorf("%w after %d steps (max %d) without a finish call", ErrStepCap, step, e.stepLimit())
		}

		req := provider.Request{
			Model:        model,
			System:       e.system(),
			Messages:     messages,
			Tools:        defs,
			MaxTokens:    e.maxTokens(),
			RequireLocal: needsLocal(item.Item.Needs),
			RequireHost:  slices.Contains(item.Item.Needs, "strict_local"),
		}

		resp, decision, err := e.complete(ctx, req, meter, p)
		if err != nil {
			// The run was cancelled while a request or a backoff was in flight.
			// Return the context's own error: the agent loop checks ctx.Err()
			// and deliberately does not report an interrupted run, leaving it to
			// the reaper.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return meter.result(""), ctxErr
			}
			if errors.Is(err, ErrTokenBudget) {
				return meter.result(err.Error()), err
			}

			summary := fmt.Sprintf("run failed at step %d: endpoint %s (provider %s) returned a %s error; %s",
				step+1, item.Endpoint.Name, p.Name(), decision.Class, decision.Reason)
			e.logf("run %s: %s: %v", item.RunID, summary, err)

			// decision.Reason is the operator-facing half of the decision, so it
			// travels on the error the agent reports. PlacementBug is a bug
			// report, not a runtime condition: it is named as one and is never
			// retried (provider.DecideRetry already refuses context_overflow).
			bug := ""
			if decision.PlacementBug {
				bug = "; this is a placement bug, not a runtime condition"
			}
			return meter.result(summary), fmt.Errorf(
				"agent: endpoint %s (provider %s) failed (%s)%s: %s: %w",
				item.Endpoint.Name, p.Name(), decision.Class, bug, decision.Reason, err)
		}

		// Reserve-then-settle can land over the cap when a response reports more
		// tokens than the estimate reserved. The next reservation would catch
		// it, but if this response calls finish the run would otherwise end in
		// success with a silent breach — the token-side cousin of the truncated
		// success the caps exist to prevent. The last reply is not used.
		if meter.overBudget() {
			summary := fmt.Sprintf("run stopped by the token budget after settling: %d tokens spent against a %d-token budget; the last reply was not used",
				meter.spent, meter.limit)
			return meter.result(summary), fmt.Errorf(
				"%w after settling: %d tokens spent against the %d-token budget; the run fails rather than finishing over budget",
				ErrTokenBudget, meter.spent, meter.limit)
		}

		// The assistant turn must carry the calls themselves, not just their
		// ids: every family replays them (OpenAI assistant.tool_calls,
		// Anthropic tool_use blocks), and provider.Message documents that.
		messages = append(messages, provider.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		if len(resp.ToolCalls) == 0 {
			// A model that answers with prose has not finished the run. Nudge it
			// once per step; the step cap is what bounds a model that keeps
			// doing this.
			messages = append(messages, provider.Message{Role: "user", Content: noToolCallNudge})
			continue
		}

		for _, call := range resp.ToolCalls {
			if err := ctx.Err(); err != nil {
				return meter.result(""), err
			}
			tool, ok := Find(call.Name)
			if !ok {
				// An unknown tool is a model mistake the next step can fix. The
				// answer names the closed set rather than failing the run.
				messages = append(messages, provider.Message{
					Role:    "tool",
					Name:    call.Name,
					CallID:  call.ID,
					Content: unknownToolMessage(call.Name),
				})
				continue
			}

			out, terr := tool.Run(ctx, sb, call.Arguments)
			if IsFinish(terr) {
				// finish is the control signal, not a failure: the output is the
				// operator-facing summary, and feeding ErrFinished back to the
				// model would be an infinite loop (Task 11's hand-off 8).
				return meter.result(out), nil
			}
			// A cancelled context mid-tool is an interrupted run, not a tool
			// error to show the model: return promptly so the agent does not
			// report work the reaper may requeue.
			if err := ctx.Err(); err != nil {
				return meter.result(""), err
			}

			messages = append(messages, provider.Message{
				Role:    "tool",
				Name:    call.Name,
				CallID:  call.ID,
				Content: toolResultContent(call.Name, out, terr),
			})
		}
	}
}

// providerFor builds the adapter for the endpoint the claim names and returns
// the wire model id to send. The credential is resolved from THIS node's
// environment by name, after provider.CheckAuthRef: the claim payload is an
// input like any other, and a name outside the provider-key namespace is a
// refusal, not a lookup. Both the manager's probe and this path apply the same
// rule, which is why it lives in the provider package rather than here.
//
// A construction failure names the endpoint, not just the family: "provider
// openai failed (auth)" cannot tell an operator whose key failed, while
// "endpoint laptop (provider openai) failed" can.
func (e *AgenticExecutor) providerFor(item board.ClaimedItem) (provider.Provider, string, error) {
	ep := item.Endpoint
	switch {
	case strings.TrimSpace(ep.Provider) == "":
		return nil, "", fmt.Errorf("agent: the claim for run %s carries no endpoint provider", item.RunID)
	case strings.TrimSpace(ep.BaseURL) == "":
		return nil, "", fmt.Errorf("agent: endpoint %q carries no base URL in the claim", ep.Name)
	case strings.TrimSpace(item.Model.ModelID) == "":
		return nil, "", fmt.Errorf("agent: the claim for run %s carries no model id", item.RunID)
	}

	cfg := provider.Config{Provider: ep.Provider, BaseURL: ep.BaseURL, Reach: ep.Reach}
	if ep.AuthRef != "" {
		if err := provider.CheckAuthRef(ep.AuthRef); err != nil {
			return nil, "", fmt.Errorf("agent: endpoint %q: %w", ep.Name, err)
		}
		key := e.env(ep.AuthRef)
		if key == "" {
			return nil, "", fmt.Errorf(
				"agent: endpoint %q: auth_ref %q names a provider key that is not set in this node's environment",
				ep.Name, ep.AuthRef)
		}
		cfg.APIKey = key
	}

	ctor := e.newProvider
	if ctor == nil {
		ctor = e.Registry.New
	}
	p, err := ctor(cfg)
	if err != nil {
		return nil, "", fmt.Errorf("agent: endpoint %q: %w", ep.Name, err)
	}
	if p == nil {
		return nil, "", fmt.Errorf("agent: endpoint %q: provider constructor returned no provider", ep.Name)
	}
	return p, item.Model.ModelID, nil
}

func (e *AgenticExecutor) env(name string) string {
	if e.lookupEnv != nil {
		return e.lookupEnv(name)
	}
	return os.Getenv(name)
}

// complete sends one request through the retry policy. The reservation and the
// settlement are per attempt, not per run: a retried Complete can be billed
// twice, so an attempt that fails keeps its reservation charged and the next
// attempt must reserve again (Task 6's hand-off 5).
func (e *AgenticExecutor) complete(ctx context.Context, req provider.Request, meter *tokenMeter, p provider.Provider) (provider.Response, provider.RetryDecision, error) {
	var resp provider.Response
	decision, err := provider.Retry(ctx, e.wait, func(attemptCtx context.Context) error {
		estimate := p.EstimateTokens(req)
		if estimate < 0 {
			estimate = 0
		}
		if rerr := meter.reserve(estimate, req.MaxTokens); rerr != nil {
			return rerr
		}
		r, cerr := p.Complete(attemptCtx, req)
		if cerr != nil {
			// A failed attempt reported no usage (the seam permits an empty
			// Usage only on failure), so the conservative settlement is the
			// reservation: the endpoint may have processed the prompt before it
			// failed, and treating an unknown cost as free is how a budget
			// becomes a suggestion.
			meter.settleFailure()
			return cerr
		}
		meter.settleSuccess(r.Usage)
		resp = r
		return nil
	})
	return resp, decision, err
}

// wait is the retry policy's cancellable backoff. It never sleeps on the wall
// clock; a timer on the run's context is what makes a shutdown prompt.
func (e *AgenticExecutor) wait(ctx context.Context, d time.Duration) error {
	if e.sleep != nil {
		return e.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (e *AgenticExecutor) check(item board.ClaimedItem) error {
	switch {
	case e.Registry == nil && e.newProvider == nil:
		return errors.New("agent: AgenticExecutor has neither a Registry nor a provider constructor")
	case e.SandboxRoot == "":
		return errors.New("agent: AgenticExecutor has no SandboxRoot")
	case item.RunID == "":
		return errors.New("agent: AgenticExecutor was given a claim with no run id, so there is no run directory to create")
	}
	return nil
}

func (e *AgenticExecutor) stepLimit() int {
	if e.MaxSteps > 0 {
		return e.MaxSteps
	}
	return defaultMaxSteps
}

func (e *AgenticExecutor) maxTokens() int {
	if e.MaxTokens > 0 {
		return e.MaxTokens
	}
	return defaultMaxTokens
}

func (e *AgenticExecutor) tokenLimit() int {
	if e.TokenBudget > 0 {
		return e.TokenBudget
	}
	return defaultTokenBudget
}

func (e *AgenticExecutor) system() string {
	if strings.TrimSpace(e.System) != "" {
		return e.System
	}
	return defaultSystemPrompt
}

// logKernelOnce reports the machine's kernel-confinement state exactly once per
// executor. On a node without it, the tool layer is the only boundary and the
// operator must be told so before trusting any shell claim in the run's summary
// (Task 10/11's hand-off 3).
func (e *AgenticExecutor) logKernelOnce() {
	e.kernelOnce.Do(func() {
		ok, reason := KernelConfinement()
		if ok {
			e.logf("kernel confinement is active: child processes are confined by the OS")
			return
		}
		e.logf("kernel confinement is NOT active: %s; the tool layer is the only boundary on this run", reason)
	})
}

func (e *AgenticExecutor) logf(format string, args ...any) {
	if e.logFn != nil {
		e.logFn(format, args...)
		return
	}
	log.Printf("agent: "+format, args...)
}

// tokenMeter is one run's budget. reserved is the outstanding reservation for
// the attempt in flight; spent is everything already settled. A reservation is
// counted against the cap before the request is sent, which is the only point
// at which refusing is still free.
type tokenMeter struct {
	limit    int
	spent    int
	reserved int
	// charged is the sum of failed attempts' reservations. It is reported
	// separately from in/out because it is a budget charge, not a measurement.
	charged int

	// in and out are the provider-reported actuals. They are what the run
	// reports to the board and are deliberately not inflated by a failed
	// attempt's reservation: the reservation is a budget guard, not a
	// measurement.
	in  int
	out int
}

func (m *tokenMeter) reserve(estimatedPrompt, maxOutput int) error {
	reservation := estimatedPrompt + maxOutput
	if reservation < 1 {
		reservation = 1
	}
	remaining := m.limit - m.spent - m.reserved
	if remaining < 0 {
		// A previous request's actual usage can exceed its estimate, so the
		// meter may already be past the limit when the next request is built.
		// Say "none remains" rather than printing a negative number.
		remaining = 0
	}
	if reservation > remaining {
		return fmt.Errorf(
			"%w: the next request needs %d tokens (an estimated %d-token prompt plus %d for the reply) and only %d of the %d-token budget remains; the run stops before sending it",
			ErrTokenBudget, reservation, estimatedPrompt, maxOutput, remaining, m.limit)
	}
	m.reserved += reservation
	return nil
}

func (m *tokenMeter) settleSuccess(usage provider.Usage) {
	m.reserved = 0
	in, out := nonNegative(usage.In), nonNegative(usage.Out)
	m.spent += in + out
	m.in += in
	m.out += out
}

func (m *tokenMeter) settleFailure() {
	m.spent += m.reserved
	m.charged += m.reserved
	m.reserved = 0
}

// overBudget reports the one case a pre-request reservation cannot prevent: the
// response settled above the limit because the estimate ran low. The caller
// fails the run rather than using the reply.
func (m *tokenMeter) overBudget() bool { return m.spent > m.limit }

func (m *tokenMeter) result(summary string) Result {
	return Result{Summary: summary, TokensIn: m.in, TokensOut: m.out, ReservedTokens: m.charged}
}

// nonNegative keeps a provider that reports a negative count from lowering the
// spend: the seam documents non-negative usage, and a budget that can be talked
// down is not a budget.
func nonNegative(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// userBrief is the one user turn the run starts from. The brief is the only
// prose the operator gives the worker; the title is included because it is the
// human-readable name of the work.
func userBrief(item board.ClaimedItem) string {
	var b strings.Builder
	if title := strings.TrimSpace(item.Item.Title); title != "" {
		fmt.Fprintf(&b, "Task: %s\n\n", title)
	}
	b.WriteString(item.Brief)
	if strings.TrimSpace(b.String()) == "" {
		b.WriteString("(no brief or title was supplied; call finish and say what you could not do)")
	}
	return b.String()
}

// needsLocal maps the item's declarations onto the provider's last-line egress
// check. Placement already refuses a no_egress item on a wan endpoint; this is
// the request half of the double-check that adapter performs.
func needsLocal(needs []string) bool {
	return slices.Contains(needs, "no_egress") || slices.Contains(needs, "strict_local")
}

// unknownToolMessage is the answer to a hallucinated tool: name the mistake and
// the closed set, so the next step can correct it.
func unknownToolMessage(name string) string {
	names := make([]string, 0, len(Tools()))
	for _, tool := range Tools() {
		names = append(names, tool.Name)
	}
	return fmt.Sprintf("agent: unknown tool %q. The tools are: %s. Call one of them.", name, strings.Join(names, ", "))
}

// toolResultContent is what the model reads for one tool call. On failure it
// carries the output AND the error: a compiler's stderr and "exit status 1" are
// one fact, and showing only the error throws away the diagnostics. finish
// never reaches this function — it is the loop's control signal.
func toolResultContent(name, out string, err error) string {
	content := out
	if err != nil {
		if content == "" {
			content = err.Error()
		} else {
			content += "\n" + err.Error()
		}
	}
	if strings.TrimSpace(content) == "" {
		content = fmt.Sprintf("agent: %s returned no output", name)
	}
	if len(content) > maxToolOutputBytes {
		return fmt.Sprintf(
			"agent: %s produced %d bytes of output, which is over the %d-byte tool-output cap, so it was not put into the conversation: it would spend the run's context on text you cannot read. Read the file in parts, list a narrower directory, or use shell to extract only what you need.",
			name, len(content), maxToolOutputBytes)
	}
	return content
}
