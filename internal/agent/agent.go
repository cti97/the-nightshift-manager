package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/board"
)

// Defaults from spec §10: heartbeat every 15s, claim every 25s.
const (
	defaultHeartbeat = 15 * time.Second
	defaultPoll      = 25 * time.Second
)

// freeSlots is what this agent asks for and advertises. M1 executes one run at
// a time; the node's concurrency on the board is a separate, server-side fact.
const freeSlots = 1

// Report is the one call the loop retries. board.Report is idempotent by design
// — the first report wins and later ones are answered with applied=false — so a
// retry cannot settle a run twice, while a dropped report would discard a
// completed run: the lease then expires, the reaper requeues the item, the work
// executes a second time and its tokens are never recorded. That asymmetry is
// why the window is generous rather than tight: five tries over roughly thirty
// seconds rides out a network blip, and the idempotency is what makes waiting
// safe.
//
// reportAttempts counts total tries, not retries.
const (
	reportAttempts    = 5
	reportBackoffBase = 2 * time.Second
	reportBackoffCap  = 15 * time.Second
)

type Config struct {
	BaseURL  string
	Token    string
	NodeID   string // used to label log lines; the board identifies the node by Token
	Executor Executor
	Protocol int // 0 means the protocol this binary speaks
	Provides []string
	Reaches  []string
	// Heartbeat and Poll default to 15s and 25s (spec §10).
	Heartbeat time.Duration
	Poll      time.Duration
	// Canary is the steward's watch on the endpoints it can reach from its own network path. Zero
	// Interval disables it: a node that is not always on has no business demoting endpoints.
	Canary CanarySettings
	// Lan is the services this node promises are reachable from inside the home network. They are
	// probed on every heartbeat and declared only while they answer (spec §8): a cached capability
	// is one the node stopped having when its owner closed the lid.
	Lan []LanProbe
}

type Agent struct {
	cfg Config
	c   *Client

	// Report retry policy. It is per-Agent rather than read from the package
	// constants at call time so a test can shrink the window without sleeping
	// for the real thirty seconds, and so two agents cannot share mutable
	// policy. New fills it from the constants above.
	reportTries        int
	reportBackoff      time.Duration // first wait; doubles per retry
	reportBackoffLimit time.Duration // cap on any single wait

	// lastStatus is the last heartbeat status already logged. Only the
	// heartbeat goroutine writes it after Run's initial synchronous call, so it
	// needs no lock; it exists so an incompatible node says so once, not every
	// 15s.
	lastStatus string
}

func New(cfg Config) *Agent {
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = defaultHeartbeat
	}
	if cfg.Poll <= 0 {
		cfg.Poll = defaultPoll
	}
	if cfg.Protocol == 0 {
		// Sending zero would have the board mark the node incompatible: a
		// protocol version is not a zero value.
		cfg.Protocol = board.ProtocolMax
	}
	return &Agent{
		cfg:                cfg,
		c:                  NewClient(cfg.BaseURL, cfg.Token),
		reportTries:        reportAttempts,
		reportBackoff:      reportBackoffBase,
		reportBackoffLimit: reportBackoffCap,
	}
}

// reportWait is the backoff before retry n (1-based): the base doubled each
// time and capped. The cap keeps any single wait from outgrowing the window the
// retry budget is meant to fit in; with the defaults the schedule is 2s, 4s,
// 8s, 15s — five tries over roughly half a minute.
func (a *Agent) reportWait(retry int) time.Duration {
	wait := a.reportBackoff
	for i := 1; i < retry; i++ {
		if wait >= a.reportBackoffLimit {
			return a.reportBackoffLimit
		}
		wait *= 2
	}
	if wait > a.reportBackoffLimit {
		wait = a.reportBackoffLimit
	}
	return wait
}

// Run is the loop from spec §10: heartbeat, claim, start, execute, report. It
// returns only when ctx is cancelled (or when the configuration cannot work),
// and it returns promptly: every wait is on ctx and every board call carries
// it.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.check(); err != nil {
		return err
	}

	// Registration deliberately leaves provides_live empty: a node that has
	// never heartbeated can do nothing. The first heartbeat is therefore sent
	// synchronously, before the first claim, so the node is eligible by the
	// time it asks for work.
	// The executor is built from configuration, before a client exists. Filing needs the client, so
	// it is attached here — once, before any run — rather than threaded through every Run call.
	if f, ok := a.cfg.Executor.(interface{ attachFiler(filingClient) }); ok {
		f.attachFiler(a.c)
	}

	a.heartbeat(ctx)

	// Heartbeats run on their own goroutine, not once per claim cycle. A run
	// that takes longer than the 300s lease would otherwise be reaped while it
	// is still executing, and its result would arrive for a run the board had
	// already settled.
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		a.heartbeatLoop(ctx)
	}()
	// Wait for the heartbeat goroutine before returning: Run's contract is that
	// nothing of this agent is still running when it returns.
	defer func() { <-hbDone }()

	// The canary runs beside the heartbeat, on its own clock: it is a duty of being always on, not
	// part of the claim loop, and a slow probe must never delay a claim.
	canaryDone := make(chan struct{})
	if a.cfg.Canary.Interval > 0 {
		go func() {
			defer close(canaryDone)
			newCanary(a.c, a.cfg.Canary).Loop(ctx, a.logf)
		}()
	} else {
		close(canaryDone)
	}
	defer func() { <-canaryDone }()

	poll := time.NewTicker(a.cfg.Poll)
	defer poll.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		items, err := a.c.Claim(ctx, freeSlots)
		if err != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			// A refusal is not retried immediately. The loop waits below, so a
			// board that is unhappy sees one request per poll, not a hot loop.
			a.logf("claim: %v", err)
		}
		for _, it := range items {
			a.execute(ctx, it)
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
		}
	}
}

func (a *Agent) check() error {
	switch {
	case a.cfg.BaseURL == "":
		return errors.New("agent: BaseURL is required")
	case a.cfg.Token == "":
		return errors.New("agent: Token is required")
	case a.cfg.NodeID == "":
		return errors.New("agent: NodeID is required")
	case a.cfg.Executor == nil:
		return errors.New("agent: Executor is required")
	}
	// A typo in the board URL would otherwise mean a process that logs a
	// connection error every poll, forever. Fail before the loop starts.
	u, err := url.Parse(a.cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("agent: BaseURL %q is not an http(s) URL", a.cfg.BaseURL)
	}
	return nil
}

func (a *Agent) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(a.cfg.Heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.heartbeat(ctx)
		}
	}
}

// heartbeat re-states what this node can do, as of now. provides_live is sent
// on every beat: the board stores the live set, and an empty one here would
// overwrite the registration on purpose (the node can then claim nothing).
func (a *Agent) heartbeat(ctx context.Context) {
	// The confinement capability is appended to the declared set on every beat, from the probe
	// rather than from configuration: what this agent can promise about the boundary is a fact
	// about the machine, and a stale claim here is a run placed on a sandbox that is not there.
	// The declared set is rebuilt every beat from facts about the machine — the confinement probe
	// and the LAN probes — never from what was declared last time.
	caps := append([]string{}, a.cfg.Provides...)
	caps = append(caps, SandboxCapability()...)
	caps = append(caps, LanCapabilities(ctx, a.cfg.Lan)...)
	resp, err := a.c.Heartbeat(ctx, board.HeartbeatRequest{
		ProvidesLive: caps,
		ReachClasses: a.cfg.Reaches,
		FreeSlots:    freeSlots,
		Protocol:     a.cfg.Protocol,
	})
	if err != nil {
		if ctx.Err() == nil { // a cancelled call is shutdown, not a failure
			a.logf("heartbeat: %v", err)
		}
		return
	}
	a.noteStatus(resp)
}

// noteStatus reports a status the board has newly put this node in. Online is
// the normal state and only worth a line when it changes; incompatible is the
// one that silently stops all work, so it must not be invisible.
func (a *Agent) noteStatus(resp board.HeartbeatResponse) {
	if resp.Status == a.lastStatus {
		return
	}
	switch resp.Status {
	case "online":
		a.logf("online: %d queued, %d awaiting approval", resp.Queued, resp.Approvals)
	case "incompatible":
		a.logf("%s", incompatibleMessage(resp, a.cfg.Protocol))
	default:
		a.logf("heartbeat status %q; it can claim nothing", resp.Status)
	}
	a.lastStatus = resp.Status
}

// incompatibleMessage says what is true about a mismatch. The board reports the
// range it speaks on every heartbeat response; a protocol-1 board does not, and
// then the only honest sentence is that the range was not reported. Printing
// this binary's own ProtocolMin..Max as if it were the board's is the lie this
// function exists to remove: a new node against an old board would otherwise
// log "board speaks 1..2" at exactly the moment the operator needs the truth.
func incompatibleMessage(resp board.HeartbeatResponse, nodeProtocol int) string {
	if resp.ProtocolMin > 0 && resp.ProtocolMax >= resp.ProtocolMin {
		return fmt.Sprintf(
			"board marked this node incompatible (node speaks protocol %d, board speaks %d..%d); it can claim nothing",
			nodeProtocol, resp.ProtocolMin, resp.ProtocolMax)
	}
	return fmt.Sprintf(
		"board marked this node incompatible (node speaks protocol %d; the board did not report its supported range); it can claim nothing",
		nodeProtocol)
}

// execute runs one claim: start the run, delegate, report. A refused start is
// not retried — the run is gone or not ours, and Start is not idempotent — but
// a failed report is, because Report is idempotent by design: see report.
func (a *Agent) execute(ctx context.Context, it board.ClaimedItem) {
	if it.RunID == "" {
		a.logf("claim for item %s carries no run id; skipping", it.Item.ID)
		return
	}
	a.logf("claimed item %s attempt %d (run %s): %s",
		it.Item.ID, it.Item.Attempt, it.RunID, it.PlacementReason)
	if err := a.c.Start(ctx, it.RunID); err != nil {
		a.logf("start run %s: %v", it.RunID, err)
		return
	}

	res, runErr := a.cfg.Executor.Run(ctx, it)

	// Shutdown interrupted the run. Do not report: a report with a dead
	// context is rejected, and marking interrupted work failed would be a lie
	// about work the reaper may requeue. Leaving the run non-terminal is
	// deliberate — the lease expires and the reaper settles it.
	if ctx.Err() != nil {
		a.logf("run %s interrupted; leaving it to the reaper", it.RunID)
		return
	}

	req := board.ReportRequest{
		Status:         "ok",
		Summary:        res.Summary,
		TokensIn:       res.TokensIn,
		TokensOut:      res.TokensOut,
		ReservedTokens: res.ReservedTokens,
	}
	// A run that ended by asking a human is parked, not reported: the board has
	// its own transition for it, and reporting it as ok or error would settle the
	// item and lose the question. The cost travels with the escalation, so the
	// tokens this run spent are still charged to the day.
	if res.Approval != nil {
		a.escalate(ctx, it.RunID, res)
		return
	}
	if runErr != nil {
		req.Status = "error"
		req.Error = runErr.Error()
	}
	a.report(ctx, it.RunID, req)
}

// report sends the run's result, retrying a bounded number of times. Only a
// failed report is retried; Claim and Start are not, because neither is
// idempotent and a retry could double-claim or start a settled run.
//
// The wait is cancellable: if shutdown arrives mid-retry, the report is
// abandoned like any other interrupted step and the reaper settles the run.
func (a *Agent) report(ctx context.Context, runID string, req board.ReportRequest) {
	for attempt := 1; ; attempt++ {
		_, err := a.c.Report(ctx, runID, req)
		if err == nil {
			return
		}
		if ctx.Err() != nil { // shutdown, not a board failure
			return
		}
		if refusal, ok := terminalRefusal(err); ok {
			// A verdict, not a glitch: the board will answer the same thing every time. A stale
			// lease means this run was reaped and its item requeued, possibly to another node —
			// so the operator needs to know that a second run may have happened, and the client
			// rule is to publish nothing from this one (spec §22.1).
			if refusal.Code == "stale_lease" {
				a.logf("report run %s: the board says the lease is gone (%v); abandoning this run without a retry — publish nothing from it", runID, refusal)
			} else {
				a.logf("report run %s: the board refused it (%v); not retrying, because a 4xx is a verdict", runID, refusal)
			}
			return
		}
		if attempt >= a.reportTries {
			a.logf("report run %s: giving up after %d attempts: %v", runID, attempt, err)
			return
		}
		wait := a.reportWait(attempt)
		a.logf("report run %s (attempt %d/%d): %v; retrying in %s", runID, attempt, a.reportTries, err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// escalate posts the run's request_approval to the board, retrying like a report
// because a lost escalation would leave the run live until its lease expired.
// A run whose escalation cannot be delivered is left to the reaper rather than
// reported as ok or failed: the work did not finish, and nothing went wrong with
// it either.
func (a *Agent) escalate(ctx context.Context, runID string, res Result) {
	if res.Approval == nil {
		return
	}
	req := board.EscalationRequest{
		Action:         res.Approval.Action,
		Reason:         res.Approval.Reason,
		Options:        res.Approval.Options,
		TokensIn:       res.TokensIn,
		TokensOut:      res.TokensOut,
		ReservedTokens: res.ReservedTokens,
	}
	for attempt := 1; ; attempt++ {
		err := a.c.Escalate(ctx, runID, req)
		if err == nil {
			return
		}
		if ctx.Err() != nil { // shutdown, not a board failure
			return
		}
		if refusal, ok := terminalRefusal(err); ok {
			a.logf("escalate run %s: the board refused it (%v); not retrying, because a 4xx is a verdict", runID, refusal)
			return
		}
		if attempt >= a.reportTries {
			a.logf("escalate run %s: giving up after %d attempts: %v", runID, attempt, err)
			return
		}
		wait := a.reportWait(attempt)
		a.logf("escalate run %s (attempt %d/%d): %v; retrying in %s", runID, attempt, a.reportTries, err, wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// logf prefixes every line with the node this agent was configured as, so two
// agents' logs in one terminal stay distinguishable.
func (a *Agent) logf(format string, args ...any) {
	log.Printf("agent %s: "+format, append([]any{a.cfg.NodeID}, args...)...)
}

// terminalRefusal is a board answer that retrying cannot improve: a 4xx other than 408/429. It
// exists so report and escalate share one rule — the alternative was two hand-written checks
// that drift, and a retry loop against a stale lease is exactly the loop that turns one bad
// night into a request storm.
func terminalRefusal(err error) (*StatusError, bool) {
	var se *StatusError
	if errors.As(err, &se) && se.Terminal() {
		return se, true
	}
	return nil, false
}
