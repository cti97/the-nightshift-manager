// internal/agent/reflex.go
package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/cti97/the-nightshift-manager/internal/board"
	"github.com/cti97/the-nightshift-manager/internal/provider"
)

// ReflexExecutor is the steward's path (spec §7): claim, one model call, report. No tool loop, no
// sandbox, no steps. It exists because a Raspberry Pi at six tokens a second cannot run a
// forty-step loop — pretending otherwise produces confident garbage rather than an error (D9).
//
// What it is for is presence, not compute: keeping the no_egress lane alive at 3am for work that
// cannot go to the cloud, and answering cheap classification or extraction questions in one turn.
//
// It makes exactly one provider request. A model that answers with prose is the answer; a model
// that answers with a tool call has been given no tools to call, so the text is what comes back.
// The run cannot loop, cannot touch the filesystem, and costs what one call costs.
type ReflexExecutor struct {
	// Registry constructs the adapter the claim names; newProvider is the test seam, exactly as
	// the agentic executor has it.
	Registry    *provider.Registry
	newProvider func(provider.Config) (provider.Provider, error)
	lookupEnv   func(string) string

	// System overrides the one-turn prompt.
	System string
	// MaxTokens caps the reply. Zero selects defaultReflexMaxTokens.
	MaxTokens int
	// TokenBudget bounds the one call's settle. Zero selects defaultTokenBudget.
	TokenBudget int
}

// defaultReflexMaxTokens is smaller than the agentic default: a reflex turn is an answer, not a
// plan, and the steward's budget is a Pi's budget.
const defaultReflexMaxTokens = 1024

// reflexSystem is the one-turn instruction. It says what the run is: a single answer with no tools,
// because a model that believes it has tools will ask for them and burn its one turn doing it.
const reflexSystem = `You are a reflex worker on a small always-on machine. You get one turn and you
have no tools: no shell, no files, no network. Answer the task directly and briefly in prose. If it
cannot be answered without tools or files, say plainly what would be needed and stop.`

func (e *ReflexExecutor) Run(ctx context.Context, item board.ClaimedItem) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	p, model, err := providerDeps{Registry: e.Registry, newProvider: e.newProvider, lookupEnv: e.lookupEnv}.forItem(item)
	if err != nil {
		return Result{}, err
	}
	if err := provider.CheckAuthRef(item.Endpoint.AuthRef); err != nil && item.Endpoint.AuthRef != "" {
		return Result{}, fmt.Errorf("agent: endpoint %q: %w", item.Endpoint.Name, err)
	}

	system := e.System
	if strings.TrimSpace(system) == "" {
		system = reflexSystem
	}
	maxTokens := e.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultReflexMaxTokens
	}
	req := provider.Request{
		Model:     model,
		System:    system,
		Messages:  []provider.Message{{Role: "user", Content: userBrief(item)}},
		MaxTokens: maxTokens,
		// A reflex item is by definition small; the reach rules still apply, and a steward that
		// cannot reach the endpoint should have been refused by placement, not discovered here.
		RequireLocal: needsLocal(item.Item.Needs),
		RequireHost:  strings.Contains(strings.Join(item.Item.Needs, ","), "strict_local"),
	}

	resp, err := p.Complete(ctx, req)
	if err != nil {
		return Result{}, fmt.Errorf("agent: endpoint %q (provider %s) failed: %w", item.Endpoint.Name, p.Name(), err)
	}
	res := Result{
		Summary:  strings.TrimSpace(resp.Content),
		TokensIn: resp.Usage.In, TokensOut: resp.Usage.Out,
	}
	if res.Summary == "" {
		// An empty answer is a failed run, not an empty summary: reporting "done" with nothing is
		// the exact failure the board's result_summary exists to prevent.
		return res, fmt.Errorf("agent: endpoint %q returned an empty answer", item.Endpoint.Name)
	}
	limit := e.TokenBudget
	if limit <= 0 {
		limit = defaultTokenBudget
	}
	if res.TokensIn+res.TokensOut > limit {
		return res, fmt.Errorf("%w: the reflex answer used %d tokens against a %d-token budget",
			ErrTokenBudget, res.TokensIn+res.TokensOut, limit)
	}
	return res, nil
}
