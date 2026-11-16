package agent

import (
	"context"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

// Result is what one execution produced. The token counts are recorded on the
// run for the spend view; the board never interprets them.
//
// TokensIn and TokensOut are provider-reported actuals. ReservedTokens is the
// budget charged for attempts that failed and reported no usage: a retried
// Complete can be billed twice, and the final attempt's usage does not show the
// earlier charge. It is reported separately rather than folded into the
// measurements so the spend view can count both without pretending the failed
// attempt produced output.
type Result struct {
	Summary        string
	TokensIn       int
	TokensOut      int
	ReservedTokens int

	// Approval is set when the run ended by asking a human (M3). It is not a
	// failure and not a summary: the caller escalates the item instead of
	// reporting the run, and the token counts above are what the board prices
	// into the day's ledger when it parks the item.
	Approval *board.ApprovalRequest
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
