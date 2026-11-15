package provider

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// This file is the retry policy the taxonomy exists for (design §10, §16). It is
// deliberately three small pieces — a decision, a schedule, and a loop whose
// wait is injected — so it can be proved without sleeping and so the decision can
// be reused by anything that needs to label a failure without retrying it.
//
// The split that makes the decision correct is the one the adapters guarantee:
// ErrTransport is single-valued ("the transport failed on its own"), and a
// context cancellation or deadline is returned as the context's own error, never
// wrapped in ErrTransport (see openai.go's do). The policy therefore keys on both
// halves: a transport failure under a live caller context is transient, while any
// failure seen after the caller's context ended is not re-sent into it. Keying on
// ErrTransport alone re-sends a shutdown's request; keying on context errors as
// "never retry" drops the ordinary unreachable-server case that a retry fixes.

// ErrorClass is one bucket a provider error normalizes to. The seven taxonomy
// classes keep their design §10 names; the rest name conditions the taxonomy
// deliberately excludes (ErrEgressRefused, ErrScriptExhausted) or context
// endings, which are not model-protocol failures but must still be decided.
type ErrorClass string

const (
	ClassNone             ErrorClass = "none"
	ClassAuth             ErrorClass = "auth"
	ClassRateLimited      ErrorClass = "rate_limited"
	ClassContextOverflow  ErrorClass = "context_overflow"
	ClassModelMissing     ErrorClass = "model_missing"
	ClassTransport        ErrorClass = "transport"
	ClassBadRequest       ErrorClass = "bad_request"
	ClassServer           ErrorClass = "server_error"
	ClassEgressRefused    ErrorClass = "egress_refused"
	ClassScriptExhausted  ErrorClass = "script_exhausted"
	ClassCanceled         ErrorClass = "canceled"
	ClassDeadlineExceeded ErrorClass = "deadline_exceeded"
	ClassUnknown          ErrorClass = "unknown"
)

// RetryDecision is the policy's verdict on one error. Retry is the actionable
// part; Class and Reason are what a run's failure event reports, and
// PlacementBug marks the one class that is a bug report rather than a provider
// fault.
type RetryDecision struct {
	Class ErrorClass
	Retry bool
	// PlacementBug is set only for context_overflow: the request did not fit,
	// which means placement computed the token fit wrongly (design §9). A caller
	// that surfaces Reason as the failure reason is attaching exactly the bug
	// report the operator needs.
	PlacementBug bool
	// Reason is printable, one line, and says why the decision went the way it
	// did. It never parses an error message; it is built from the class.
	Reason string
}

// DecideRetry classifies err and says whether it is worth another attempt.
// runCtx must be the run's own context — the one whose ending means "stop this
// run" — never an adapter's per-attempt timeout context. The name is deliberate:
// an executor that passes a context it created with WithTimeout around one call
// makes every per-request timeout look like a shutdown, and the deadline row
// below silently becomes "no retry". runCtx is consulted only for whether it has
// ended, so the decision is deterministic, clock-free and blocks on nothing; it
// is not "pure" in the strict sense because it reads that one fact from runCtx.
// A nil runCtx is treated as "no caller signal", which is useful only to a
// caller that classifies without one.
//
// The taxonomy's table (design §10) is exactly: transport, server_error and
// rate_limited retry; auth, model_missing, bad_request and context_overflow do
// not. context.DeadlineExceeded retries only while runCtx is still live, because
// then it must be the adapter's per-request timeout — openai.go's do() documents
// a Config.Timeout expiry as "a legitimate timeout rather than a cancellation" —
// and stops once runCtx itself has ended, because then it is the run's own
// deadline. An error whose class nothing recognizes is not retried: failing
// closed is the only defensible verdict on an error this policy cannot reason
// about.
func DecideRetry(runCtx context.Context, err error) RetryDecision {
	if err == nil {
		return RetryDecision{Class: ClassNone}
	}

	decision := classifyProviderError(err)

	// The run's context is the veto, and it is checked after classification so
	// the class still names what went wrong when a retry is refused. This is the
	// half a policy keyed on ErrTransport alone is missing: a transport failure
	// observed as the run shuts down is not evidence that anyone wants the
	// request re-sent.
	if callerErr := callerContextError(runCtx); callerErr != nil && decision.Retry {
		decision.Retry = false
		decision.Reason = fmt.Sprintf("the caller's context ended (%v) before the retry, so a %s failure is not re-sent into it", callerErr, decision.Class)
	}
	return decision
}

// classifyProviderError maps err to its class and its reason. Context endings
// are checked before the sentinels so that an error carrying both (which the
// adapters do not produce, but a wrapper could) is judged by the ending: the
// safe direction is to stop.
func classifyProviderError(err error) RetryDecision {
	switch {
	case errors.Is(err, context.Canceled):
		return RetryDecision{
			Class:  ClassCanceled,
			Reason: "the context was canceled; the work is no longer wanted, so nothing is re-sent",
		}

	case errors.Is(err, context.DeadlineExceeded):
		// Whether this is a legitimate per-request timeout or the run's own
		// deadline is decided by the caller's context in DecideRetry; the
		// reason here is the optimistic reading, replaced if the context has
		// ended.
		return RetryDecision{
			Class:  ClassDeadlineExceeded,
			Retry:  true,
			Reason: "a per-request timeout expired while the caller's context is still live; this is a legitimate timeout, not a cancellation",
		}

	case errors.Is(err, ErrAuth):
		return RetryDecision{
			Class:  ClassAuth,
			Reason: "the credential is wrong; retrying a 401 would spend the same failure again (design §16)",
		}

	case errors.Is(err, ErrRateLimited):
		return RetryDecision{
			Class:  ClassRateLimited,
			Retry:  true,
			Reason: "the endpoint asked us to slow down; the bounded backoff is exactly what that signal asks for",
		}

	case errors.Is(err, ErrContextOverflow):
		return RetryDecision{
			Class:        ClassContextOverflow,
			PlacementBug: true,
			Reason:       "the request did not fit, so placement computed the token fit wrongly (design §9); this is a placement bug, not a runtime condition — fix the fit, do not retry",
		}

	case errors.Is(err, ErrModelMissing):
		return RetryDecision{
			Class:  ClassModelMissing,
			Reason: "the endpoint does not serve this model; no retry makes a missing model appear",
		}

	case errors.Is(err, ErrTransport):
		return RetryDecision{
			Class:  ClassTransport,
			Retry:  true,
			Reason: "the transport failed on its own; a retry can reach a server that is reachable again",
		}

	case errors.Is(err, ErrBadRequest):
		return RetryDecision{
			Class:  ClassBadRequest,
			Reason: "the request is malformed; re-sending the same bytes cannot succeed",
		}

	case errors.Is(err, ErrServer):
		return RetryDecision{
			Class:  ClassServer,
			Retry:  true,
			Reason: "a server error is transient; a retry can land on a healthy instance or after an overload clears",
		}

	case errors.Is(err, ErrEgressRefused):
		// Outside the taxonomy, and deliberately its own class rather than a
		// transport failure: the bytes were never sent, the endpoint's declared
		// reach cannot carry them, and no retry changes a declaration. Retrying
		// it would hammer a policy that will never change.
		return RetryDecision{
			Class:  ClassEgressRefused,
			Reason: "the request requires a local endpoint and the endpoint's declared reach is not local; the configuration is wrong, so a retry would hammer a policy that will never change",
		}

	case errors.Is(err, ErrScriptExhausted):
		return RetryDecision{
			Class:  ClassScriptExhausted,
			Reason: "the stub's script has no response left; a retry would exhaust the same script again",
		}

	default:
		return RetryDecision{
			Class:  ClassUnknown,
			Reason: "the error is not one of the provider taxonomy's classes or a context ending; failing closed rather than retrying an error this policy does not understand",
		}
	}
}

func callerContextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// MaxAttempts is the total number of provider calls one run may make for a
// transient class: the first send plus MaxAttempts-1 retries. Design §10's
// "bounded 3×" is read as three attempts, not three retries — an attempt that
// already cost a round trip is the expensive unit, and the existing agent loop
// counts its report window the same way (see internal/agent's reportAttempts).
const MaxAttempts = 3

const (
	// retryBackoffBase is the first wait. Two seconds is long enough for a
	// refused connection or an overloaded endpoint to change, and short enough
	// that three attempts fit inside a run's patience.
	retryBackoffBase = 2 * time.Second
	// retryBackoffCap bounds any single wait, so a caller that asks Backoff for
	// a later retry than this policy grants still gets a bounded answer.
	retryBackoffCap = 15 * time.Second
)

// MaxRetryWait is the most a run can spend waiting between the attempts
// MaxAttempts allows, assuming the caller-supplied sleep honours the duration it
// is given. It is the sum of the waits, not a single one, so a budget can state
// the retry loop's worst case in one number. Time spent inside the provider call
// itself is bounded separately (the adapter's per-request timeout) and is not
// part of this figure.
const MaxRetryWait = 6 * time.Second

// Backoff returns the wait before retry n (1-based): retryBackoffBase doubled
// per retry and capped. With MaxAttempts = 3 the schedule a run actually uses is
// 2s then 4s — 6s in total — and later ordinals are capped at 15s. An ordinal
// below 1 is clamped to the first wait rather than returning zero, so a caller
// that miscounts gets the conservative wait instead of a hot loop.
func Backoff(retry int) time.Duration {
	if retry < 1 {
		retry = 1
	}
	wait := retryBackoffBase
	for i := 1; i < retry; i++ {
		if wait >= retryBackoffCap {
			return retryBackoffCap
		}
		wait *= 2
	}
	if wait > retryBackoffCap {
		return retryBackoffCap
	}
	return wait
}

// SleepFunc is how a retry waits. It must return promptly when ctx ends, and it
// is supplied by the caller so this package never sleeps: a test passes a
// recorder, and the run passes a timer that is cancellable with the run. That
// injection is what makes the loop provable without a wall clock.
type SleepFunc func(ctx context.Context, d time.Duration) error

// Retry runs attempt until it succeeds, the policy says stop, or the attempt
// budget is spent, and returns the final decision plus the last provider error.
// The error is returned unchanged so the caller can still errors.Is it; the
// decision carries the class and the printable reason for the failure event.
//
// runCtx is the run's own context, and the same one is passed to attempt and to
// every wait; it must never be a per-attempt WithTimeout context. Passing one
// would be self-defeating: its deadline would already be spent when a
// per-request timeout comes back, so the policy would see an ended context and
// refuse the retry the deadline row exists to allow. The per-request timeout is
// the adapter's (Config.Timeout), and this package deliberately does not create
// one.
//
// It never calls time.Sleep: every wait goes through sleep. It also never starts
// or continues with a context that has ended — the check is at the top of the
// loop and the decision re-reads runCtx after a wait — so a shutdown is prompt
// and a retry cannot outlive the run that asked for it. A nil sleep or attempt
// is a wiring bug and panics rather than silently turning a backoff into a hot
// loop.
func Retry(runCtx context.Context, sleep SleepFunc, attempt func(context.Context) error) (RetryDecision, error) {
	if attempt == nil {
		panic("provider: Retry called with no attempt function")
	}
	if sleep == nil {
		panic("provider: Retry called with no sleep function; a retry that cannot wait would hot-loop")
	}

	var lastErr error
	for attemptNo := 1; ; attemptNo++ {
		// Re-read the context before every attempt: the wait may have ended it,
		// and the caller may have cancelled while the previous attempt ran.
		if ctxErr := callerContextError(runCtx); ctxErr != nil {
			if lastErr == nil {
				return DecideRetry(runCtx, ctxErr), ctxErr
			}
			decision := DecideRetry(runCtx, lastErr)
			decision.Retry = false
			decision.Reason = fmt.Sprintf("the caller's context ended (%v) before the next attempt, so the %s failure is not re-sent", ctxErr, decision.Class)
			return decision, lastErr
		}

		err := attempt(runCtx)
		if err == nil {
			return RetryDecision{Class: ClassNone}, nil
		}
		lastErr = err

		decision := DecideRetry(runCtx, err)
		if !decision.Retry {
			return decision, err
		}
		if attemptNo >= MaxAttempts {
			decision.Retry = false
			decision.Reason = fmt.Sprintf("%s; the %d-attempt budget is spent, so the run fails on this error instead of re-queueing it", decision.Reason, MaxAttempts)
			return decision, err
		}
		if err := sleep(runCtx, Backoff(attemptNo)); err != nil {
			// The wait was interrupted (normally by the caller's context
			// ending). The provider error is still what surfaces, but the
			// decision must not say "retry": the loop is over.
			decision.Retry = false
			decision.Reason = fmt.Sprintf("the wait before a retry was interrupted (%v); the run stops rather than retrying without its backoff", err)
			return decision, lastErr
		}
	}
}
