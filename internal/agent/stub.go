package agent

import (
	"context"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

// StubExecutor is the reference Executor: it does no work and returns a fixed
// summary. The tests and `nsm agent run` use it until M2 provides the agentic
// executor.
type StubExecutor struct {
	// OnRun, when set, replaces the default behaviour. It is how tests observe
	// or block an execution.
	OnRun func(item board.ClaimedItem) (Result, error)
}

func (s StubExecutor) Run(ctx context.Context, item board.ClaimedItem) (Result, error) {
	if s.OnRun == nil {
		return Result{Summary: "stub"}, nil
	}
	return s.OnRun(item)
}
