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
}

type Agent struct {
	cfg Config
	c   *Client

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
	return &Agent{cfg: cfg, c: NewClient(cfg.BaseURL, cfg.Token)}
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
	resp, err := a.c.Heartbeat(ctx, board.HeartbeatRequest{
		ProvidesLive: a.cfg.Provides,
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
		a.logf("board marked this node incompatible (protocol %d, board speaks %d..%d); it can claim nothing",
			a.cfg.Protocol, board.ProtocolMin, board.ProtocolMax)
	default:
		a.logf("heartbeat status %q; it can claim nothing", resp.Status)
	}
	a.lastStatus = resp.Status
}

// execute runs one claim: start the run, delegate, report. Each step is
// one-way. A refused start is not retried (the run is gone or not ours) and a
// failed report is not retried either — the first report wins, and a retry
// after an ambiguous failure would be answered idempotently by the board
// anyway.
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
		Status:    "ok",
		Summary:   res.Summary,
		TokensIn:  res.TokensIn,
		TokensOut: res.TokensOut,
	}
	if runErr != nil {
		req.Status = "error"
		req.Error = runErr.Error()
	}
	if _, err := a.c.Report(ctx, it.RunID, req); err != nil {
		a.logf("report run %s: %v", it.RunID, err)
	}
}

// logf prefixes every line with the node this agent was configured as, so two
// agents' logs in one terminal stay distinguishable.
func (a *Agent) logf(format string, args ...any) {
	log.Printf("agent %s: "+format, append([]any{a.cfg.NodeID}, args...)...)
}
