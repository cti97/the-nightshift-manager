package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestRetryableClassifiesEveryError is the policy table: every sentinel the
// taxonomy defines, the two errors that deliberately sit outside it, a context
// ending, nil, and an error nothing recognizes. Exactly transport, server_error
// and rate_limited retry; everything else fails closed, including the unknown
// class — a policy that guesses about an error it does not understand is how a
// retry loop turns a bug into a hang.
func TestRetryableClassifiesEveryError(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		wantClass    ErrorClass
		wantRetry    bool
		wantPlaceBug bool
	}{
		{name: "nil", err: nil, wantClass: ClassNone, wantRetry: false},
		{name: "auth", err: fmt.Errorf("provider: openai: HTTP 401: %w", ErrAuth), wantClass: ClassAuth},
		{
			name:      "rate_limited",
			err:       fmt.Errorf("provider: anthropic: HTTP 429: %w", ErrRateLimited),
			wantClass: ClassRateLimited,
			wantRetry: true,
		},
		{
			name:         "context_overflow",
			err:          fmt.Errorf("provider: openai: HTTP 400: %w", ErrContextOverflow),
			wantClass:    ClassContextOverflow,
			wantRetry:    false,
			wantPlaceBug: true,
		},
		{name: "model_missing", err: fmt.Errorf("provider: openai: HTTP 404: %w", ErrModelMissing), wantClass: ClassModelMissing},
		{
			name:      "transport",
			err:       fmt.Errorf("%w: dial tcp: connection refused", ErrTransport),
			wantClass: ClassTransport,
			wantRetry: true,
		},
		{name: "bad_request", err: fmt.Errorf("%w: message 0 has no role", ErrBadRequest), wantClass: ClassBadRequest},
		{
			name:      "server",
			err:       fmt.Errorf("%w: provider: openai: HTTP 503: overloaded", ErrServer),
			wantClass: ClassServer,
			wantRetry: true,
		},
		{name: "egress_refused", err: fmt.Errorf("%w: endpoint reach is \"wan\"", ErrEgressRefused), wantClass: ClassEgressRefused},
		{name: "script_exhausted", err: fmt.Errorf("%w: 3 response(s) already replayed", ErrScriptExhausted), wantClass: ClassScriptExhausted},
		{name: "unrecognized", err: errors.New("something nobody classified"), wantClass: ClassUnknown},
		{name: "canceled", err: context.Canceled, wantClass: ClassCanceled},
		// A deadline under a live caller context is the adapter's per-request
		// timeout (openai.go's do(): a Config.Timeout expiry is returned as the
		// context's own error). That is a legitimate timeout, not a shutdown.
		{name: "deadline_exceeded", err: context.DeadlineExceeded, wantClass: ClassDeadlineExceeded, wantRetry: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideRetry(context.Background(), tc.err)
			if got.Class != tc.wantClass {
				t.Fatalf("class = %q, want %q (reason: %s)", got.Class, tc.wantClass, got.Reason)
			}
			if got.Retry != tc.wantRetry {
				t.Fatalf("retry = %v, want %v (reason: %s)", got.Retry, tc.wantRetry, got.Reason)
			}
			if got.PlacementBug != tc.wantPlaceBug {
				t.Fatalf("placement bug = %v, want %v", got.PlacementBug, tc.wantPlaceBug)
			}
			if tc.wantRetry && got.Reason == "" {
				t.Fatal("a retryable decision carries no reason")
			}
			if !tc.wantRetry && tc.err != nil && got.Reason == "" {
				t.Fatalf("a non-retryable decision carries no reason; the operator needs to know why it will not be tried again")
			}
		})
	}
}

// TestContextEndingsDoNotMaskTransport is the carry-forward from the adapter
// fix. ErrTransport is single-valued — it never wraps a context ending — so the
// policy must key on both: a transport failure is retried, a canceled or
// deadline-expired caller context is not, and a transport failure seen while the
// caller's context is already dead is not re-sent into it.
func TestContextEndingsDoNotMaskTransport(t *testing.T) {
	t.Run("transport failure under a live context retries", func(t *testing.T) {
		got := DecideRetry(context.Background(), fmt.Errorf("%w: connection reset", ErrTransport))
		if !got.Retry {
			t.Fatalf("transport under a live context: retry = false, want true (reason: %s)", got.Reason)
		}
	})

	t.Run("transport failure after the caller canceled does not retry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got := DecideRetry(ctx, fmt.Errorf("%w: connection reset", ErrTransport))
		if got.Retry {
			t.Fatalf("transport under a canceled context: retry = true, want false (reason: %s)", got.Reason)
		}
		if got.Class != ClassTransport {
			t.Fatalf("class = %q, want %q: the class names the error, the decision names the context", got.Class, ClassTransport)
		}
	})

	t.Run("caller cancellation is not retried", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got := DecideRetry(ctx, fmt.Errorf("provider: openai: %w", context.Canceled))
		if got.Retry {
			t.Fatalf("canceled context: retry = true, want false (reason: %s)", got.Reason)
		}
		if got.Class != ClassCanceled {
			t.Fatalf("class = %q, want %q", got.Class, ClassCanceled)
		}
	})

	t.Run("the caller's own deadline is not retried", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
		defer cancel()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("fixture: ctx.Err() = %v, want DeadlineExceeded", ctx.Err())
		}
		got := DecideRetry(ctx, context.DeadlineExceeded)
		if got.Retry {
			t.Fatalf("run deadline: retry = true, want false (reason: %s)", got.Reason)
		}
		if got.Class != ClassDeadlineExceeded {
			t.Fatalf("class = %q, want %q", got.Class, ClassDeadlineExceeded)
		}
	})
}

// TestContextOverflowIsNotRetriedAndIsTaggedAsAPlacementBug: a context overflow
// is not a runtime condition. Placement computed the token fit (design §9), so
// the request did not fit — that is a placement bug, and the surfaced failure
// must say so rather than leaving an operator to retry it or read it as a
// provider fault.
func TestContextOverflowIsNotRetriedAndIsTaggedAsAPlacementBug(t *testing.T) {
	err := fmt.Errorf("provider: openai: model %q: HTTP 400: context_length_exceeded: %w", "gpt-4o", ErrContextOverflow)
	got := DecideRetry(context.Background(), err)
	if got.Retry {
		t.Fatalf("context_overflow: retry = true, want false (reason: %s)", got.Reason)
	}
	if !got.PlacementBug {
		t.Fatal("context_overflow: not tagged as a placement bug")
	}
	if !strings.Contains(got.Reason, "placement") {
		t.Fatalf("reason %q does not name placement; a context overflow must surface as a placement bug, not as a provider fault", got.Reason)
	}
	// The decision is about the class, so the caller's error stays inspectable:
	// a run's failure event can still report what the endpoint actually said.
	if !errors.Is(err, ErrContextOverflow) {
		t.Fatalf("the policy lost the sentinel: %v", err)
	}
}

// TestBackoffIsMonotonicCappedAndBounded pins the schedule's shape rather than
// trusting the formula: every wait is positive, never shrinks as retries
// accumulate, never exceeds the cap, and the retries the budget allows add up to
// a wait the caller can state in advance.
func TestBackoffIsMonotonicCappedAndBounded(t *testing.T) {
	if MaxAttempts < 2 {
		t.Fatalf("MaxAttempts = %d; a retry policy with no retry is not a retry policy", MaxAttempts)
	}

	prev := time.Duration(0)
	for retry := 1; retry <= MaxAttempts+8; retry++ {
		wait := Backoff(retry)
		if wait <= 0 {
			t.Fatalf("Backoff(%d) = %v, want a positive wait", retry, wait)
		}
		if wait < prev {
			t.Fatalf("Backoff(%d) = %v, shorter than Backoff(%d) = %v; the schedule must not shrink", retry, wait, retry-1, prev)
		}
		if wait > retryBackoffCap {
			t.Fatalf("Backoff(%d) = %v, above the cap %v", retry, wait, retryBackoffCap)
		}
		prev = wait
	}

	if Backoff(0) != Backoff(1) {
		t.Fatalf("Backoff(0) = %v, want the first wait %v: an ordinal below 1 is clamped, not a zero wait", Backoff(0), Backoff(1))
	}

	var total time.Duration
	for retry := 1; retry < MaxAttempts; retry++ {
		total += Backoff(retry)
	}
	if total != MaxRetryWait {
		t.Fatalf("sum of the waits the budget allows = %v, but MaxRetryWait says %v", total, MaxRetryWait)
	}
}

// TestRetryStopsOnANonRetryableClass: auth fails once. Three attempts against a
// bad credential is the "burns attempts on a bad credential" failure in §16.
func TestRetryStopsOnANonRetryableClass(t *testing.T) {
	var attempts, sleeps int
	decision, err := Retry(context.Background(),
		func(context.Context, time.Duration) error { sleeps++; return nil },
		func(context.Context) error {
			attempts++
			return fmt.Errorf("%w: HTTP 401", ErrAuth)
		},
	)
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1: auth is not retried", attempts)
	}
	if sleeps != 0 {
		t.Fatalf("slept %d time(s) before giving up on auth, want 0", sleeps)
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
	if decision.Class != ClassAuth || decision.Retry {
		t.Fatalf("decision = %+v, want class auth and retry false", decision)
	}
}

// TestRetryRetriesTransientClassesAndRecordsTheSchedule proves the waits come
// from the schedule and from the caller-supplied sleep, never from a clock in
// this package.
func TestRetryRetriesTransientClassesAndRecordsTheSchedule(t *testing.T) {
	for _, sentinel := range []error{ErrRateLimited, ErrServer, ErrTransport} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			var attempts int
			var waits []time.Duration
			decision, err := Retry(context.Background(),
				func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
				func(context.Context) error {
					attempts++
					if attempts == 2 {
						return nil
					}
					return fmt.Errorf("%w: transient", sentinel)
				},
			)
			if err != nil {
				t.Fatalf("err = %v, want nil after the retry succeeded", err)
			}
			if attempts != 2 {
				t.Fatalf("attempts = %d, want 2", attempts)
			}
			if len(waits) != 1 || waits[0] != Backoff(1) {
				t.Fatalf("waits = %v, want exactly [%v]", waits, Backoff(1))
			}
			if decision.Class != ClassNone || decision.Retry {
				t.Fatalf("decision on success = %+v, want class none and retry false", decision)
			}
		})
	}
}

// TestRetryExhaustsTheBudgetAndReportsIt: the bound is real, and the final
// decision says the budget — not the class — is why it stopped.
func TestRetryExhaustsTheBudgetAndReportsIt(t *testing.T) {
	var attempts int
	var waited time.Duration
	decision, err := Retry(context.Background(),
		func(_ context.Context, d time.Duration) error { waited += d; return nil },
		func(context.Context) error {
			attempts++
			return fmt.Errorf("%w: still down", ErrServer)
		},
	)
	if attempts != MaxAttempts {
		t.Fatalf("attempts = %d, want MaxAttempts = %d", attempts, MaxAttempts)
	}
	if waited != MaxRetryWait {
		t.Fatalf("waited %v in total, want MaxRetryWait = %v", waited, MaxRetryWait)
	}
	if decision.Retry {
		t.Fatalf("retry = true after the budget was spent (reason: %s)", decision.Reason)
	}
	if decision.Class != ClassServer {
		t.Fatalf("class = %q, want %q", decision.Class, ClassServer)
	}
	if !strings.Contains(decision.Reason, "budget") && !strings.Contains(decision.Reason, "attempt") {
		t.Fatalf("reason %q does not say the attempt budget ran out", decision.Reason)
	}
	if !errors.Is(err, ErrServer) {
		t.Fatalf("err = %v, want the last failure", err)
	}
}

// TestRetryDoesNotResendIntoACanceledContext is the bug the task exists to
// avoid: a transport failure returned as a shutdown begins must not be retried
// on a context that is already over.
func TestRetryDoesNotResendIntoACanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts, sleeps int
	decision, err := Retry(ctx,
		func(context.Context, time.Duration) error { sleeps++; return nil },
		func(context.Context) error {
			attempts++
			// The shutdown lands between the transport failing and the policy
			// deciding: exactly the race the adapter's single-valued
			// ErrTransport makes observable.
			cancel()
			return fmt.Errorf("%w: connection reset by peer", ErrTransport)
		},
	)
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1: the retry must not be re-sent into a dead context", attempts)
	}
	if sleeps != 0 {
		t.Fatalf("slept %d time(s) into a dead context, want 0", sleeps)
	}
	if decision.Retry {
		t.Fatalf("retry = true under a canceled context (reason: %s)", decision.Reason)
	}
	if !errors.Is(err, ErrTransport) {
		t.Fatalf("err = %v, want the transport error", err)
	}
}

// TestRetryStopsWhenTheWaitIsInterrupted: once the caller's context ends during
// the backoff, the retry loop is done. The last provider error is what surfaces,
// with a decision that explains the context ended.
func TestRetryStopsWhenTheWaitIsInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var attempts int
	decision, err := Retry(ctx,
		func(context.Context, time.Duration) error {
			cancel()
			return context.Canceled
		},
		func(context.Context) error {
			attempts++
			return fmt.Errorf("%w: 503", ErrServer)
		},
	)
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 after the wait was interrupted", attempts)
	}
	if decision.Retry {
		t.Fatalf("retry = true after the caller's context ended (reason: %s)", decision.Reason)
	}
	if !errors.Is(err, ErrServer) {
		t.Fatalf("err = %v, want the provider failure that caused the wait", err)
	}
}

// TestRetryStopsWhenTheWaitItselfFails: a sleep that fails while the context is
// still live is a wiring bug. Continuing would retry without the backoff it was
// asked for (or hot-loop), so the loop stops on the last provider error.
func TestRetryStopsWhenTheWaitItselfFails(t *testing.T) {
	var attempts int
	decision, err := Retry(context.Background(),
		func(context.Context, time.Duration) error { return errors.New("timer unavailable") },
		func(context.Context) error {
			attempts++
			return fmt.Errorf("%w: 503", ErrServer)
		},
	)
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1: the wait failed, so the retry must not run without it", attempts)
	}
	if decision.Retry {
		t.Fatalf("retry = true after the wait failed (reason: %s)", decision.Reason)
	}
	if !strings.Contains(decision.Reason, "interrupted") {
		t.Fatalf("reason %q does not say the wait was interrupted", decision.Reason)
	}
	if !errors.Is(err, ErrServer) {
		t.Fatalf("err = %v, want the provider failure that caused the wait", err)
	}
}

// TestRetryDoesNotStartUnderADeadContext: no attempt is made at all, and the
// caller gets the context's own error.
func TestRetryDoesNotStartUnderADeadContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	decision, err := Retry(ctx,
		func(context.Context, time.Duration) error { return nil },
		func(context.Context) error { called = true; return nil },
	)
	if called {
		t.Fatal("the attempt ran under an already-canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if decision.Retry || decision.Class != ClassCanceled {
		t.Fatalf("decision = %+v, want class canceled and retry false", decision)
	}
}
