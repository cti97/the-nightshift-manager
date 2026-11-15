package agent

import (
	"context"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

// Result is what one execution produced. The token counts are recorded on the
// run for the spend view; the board never interprets them.
type Result struct {
	Summary   string
	TokensIn  int
	TokensOut int
}

// Executor does the work. It receives the whole claim payload, so the brief —
// the only prose a worker is given — is available without the placement type
// having to carry prose.
//
// AgenticExecutor (M2+) and ReflexExecutor (M6) both implement this;
// StubExecutor is the reference implementation the tests use.
type Executor interface {
	Run(ctx context.Context, item board.ClaimedItem) (Result, error)
}
