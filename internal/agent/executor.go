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
// Run must return promptly when ctx is cancelled, and must not leave work
// running behind the context. The loop depends on that twice: prompt shutdown
// is only as prompt as the executor, and an interrupted run is deliberately
// not reported (the claim's lease expires and the reaper settles it). Work that
// outlived its cancelled context would therefore have side effects behind a run
// the board has already requeued.
//
// AgenticExecutor (M2+) and ReflexExecutor (M6) both implement this;
// StubExecutor is the reference implementation the tests use.
type Executor interface {
	Run(ctx context.Context, item board.ClaimedItem) (Result, error)
}
