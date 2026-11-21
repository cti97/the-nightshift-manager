// internal/board/threads.go
package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Thread statuses (spec §13). frozen means the next reservation did not fit and only an
// operator can lift it; closed means the turns ran out.
const (
	ThreadOpen   = "open"
	ThreadFrozen = "frozen"
	ThreadClosed = "closed"
)

// The three refusals a post can meet, plus the one a create can meet. They are sentinels
// so the HTTP layer can answer each with its own code and a caller can tell them apart
// without reading a message.
var (
	ErrThreadFrozen = errors.New("the thread is frozen: its budget is exhausted")
	ErrThreadClosed = errors.New("the thread is closed: its turns are used up")
	ErrSameAuthor   = errors.New("the same author posted twice in a row")
	ErrNoThread     = errors.New("no such thread")
)

// Thread is a conversation with a budget. The counters are stored, not derived, because
// the reservation check and the post must be one transaction: a reservation held only in a
// process's memory would let two posters each see the full budget (M5 D1).
type Thread struct {
	ID         string `json:"id"`
	WorkItemID string `json:"work_item_id,omitempty"`
	Subject    string `json:"subject"`

	BudgetTokens    int `json:"budget_tokens"`
	MaxOutputTokens int `json:"max_output_tokens"`
	TokensReserved  int `json:"tokens_reserved"`
	TokensUsed      int `json:"tokens_used"`

	MaxTurns  int    `json:"max_turns"`
	TurnsUsed int    `json:"turns_used"`
	TurnOwner string `json:"turn_owner,omitempty"`

	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
	ClosedAt  *int64 `json:"closed_at,omitempty"`
}

// Message is one posted turn. bodies are prose and are stored as given: no trimming, no
// markup, no interpretation.
type Message struct {
	ID            string `json:"id"`
	ThreadID      string `json:"thread_id"`
	Author        string `json:"author"`
	Body          string `json:"body"`
	TokensCharged int    `json:"tokens_charged"`
	CreatedAt     int64  `json:"created_at"`
}

// ThreadView is what the read returns: the row, its messages, and the one number the
// budget bar needs. Remaining is clamped at zero: a poster that overspent its reservation
// is charged the truth (D1), so tokens_used can exceed the budget, and a bar that printed
// a negative remainder would read as a bug rather than as the overspend it is.
type ThreadView struct {
	Thread
	Messages  []Message `json:"messages"`
	Remaining int       `json:"remaining"`
}

// CreateThreadRequest is the admin's thread. Defaults are applied here, not in the
// handler, so every way of creating a thread (CLI, page, test) gets the same ones.
type CreateThreadRequest struct {
	WorkItemID      string `json:"work_item_id"`
	Subject         string `json:"subject"`
	BudgetTokens    int    `json:"budget_tokens"`
	MaxOutputTokens *int   `json:"max_output_tokens"`
	MaxTurns        *int   `json:"max_turns"`
}

// defaultMaxOutput is the per-turn reservation ceiling when the caller names none: the
// second half of `reserve = estimated_prompt + max_output` (spec §13). It is a column
// rather than a constant (D2) precisely so an operator can change it per thread.
const defaultMaxOutput = 1024

// defaultMaxTurns matches the schema's default.
const defaultMaxTurns = 20

// estimateTokens is the board's reserve-only prompt estimate (D1): roughly four bytes per
// token, rounded up, minimum one. It is deliberately crude and deliberately the manager's
// number — a caller-supplied reserve can be gamed, and the reservation exists to protect a
// shared budget. It is never presented as a bill; settlement uses the poster's count.
func estimateTokens(s string) int {
	if s == "" {
		return 1
	}
	return 1 + len(s)/4
}

// CreateThread validates and stores a thread. Everything a post would otherwise discover
// at 03:00 — a subject, a budget that can hold a turn, a turn limit — is refused here.
func CreateThread(ctx context.Context, db *sql.DB, clock Clock, req CreateThreadRequest) (Thread, error) {
	subject := strings.TrimSpace(req.Subject)
	switch {
	case subject == "":
		return Thread{}, fmt.Errorf("a thread needs a subject")
	case req.BudgetTokens <= 0:
		return Thread{}, fmt.Errorf("budget_tokens must be positive: a thread with no budget can never send a message")
	}
	maxOutput := defaultMaxOutput
	if req.MaxOutputTokens != nil {
		maxOutput = *req.MaxOutputTokens
	}
	if maxOutput <= 0 {
		return Thread{}, fmt.Errorf("max_output_tokens must be positive")
	}
	maxTurns := defaultMaxTurns
	if req.MaxTurns != nil {
		maxTurns = *req.MaxTurns
	}
	if maxTurns <= 0 {
		return Thread{}, fmt.Errorf("max_turns must be positive")
	}
	// A thread that names an item must name one that exists; the foreign key would refuse
	// it anyway, and a message from SQLite's constraint is not an answer an operator can act
	// on.
	if req.WorkItemID != "" {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_items WHERE id=?`, req.WorkItemID).Scan(&n); err != nil {
			return Thread{}, err
		}
		if n == 0 {
			return Thread{}, fmt.Errorf("work_item_id %q does not exist", req.WorkItemID)
		}
	}

	now := clock.Now()
	id, err := NewToken()
	if err != nil {
		return Thread{}, err
	}
	th := Thread{
		ID:              id,
		WorkItemID:      req.WorkItemID,
		Subject:         subject,
		BudgetTokens:    req.BudgetTokens,
		MaxOutputTokens: maxOutput,
		MaxTurns:        maxTurns,
		Status:          ThreadOpen,
		CreatedAt:       now,
	}
	var itemID any
	if th.WorkItemID != "" {
		itemID = th.WorkItemID
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO threads (id,work_item_id,subject,budget_tokens,max_output_tokens,
		                     max_turns,status,created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		th.ID, itemID, th.Subject, th.BudgetTokens, th.MaxOutputTokens, th.MaxTurns, th.Status, th.CreatedAt); err != nil {
		return Thread{}, err
	}
	return th, nil
}

// GetThread reads a thread, its messages in posting order, and the remaining budget.
//
// The read is a read: it settles nothing and freezes nothing, for the /why reason — asking
// what a conversation cost must not change the answer.
func GetThread(ctx context.Context, db *sql.DB, id string) (ThreadView, error) {
	var view ThreadView
	var itemID, owner sql.NullString
	var closedAt sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT id, work_item_id, subject, budget_tokens, max_output_tokens, tokens_reserved,
		        tokens_used, max_turns, turns_used, turn_owner, status, created_at, closed_at
		   FROM threads WHERE id=?`, id).
		Scan(&view.ID, &itemID, &view.Subject, &view.BudgetTokens, &view.MaxOutputTokens,
			&view.TokensReserved, &view.TokensUsed, &view.MaxTurns, &view.TurnsUsed,
			&owner, &view.Status, &view.CreatedAt, &closedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return view, fmt.Errorf("thread %s: %w", id, ErrNoThread)
		}
		return view, fmt.Errorf("thread %s: %w", id, err)
	}
	view.WorkItemID = itemID.String
	view.TurnOwner = owner.String
	if closedAt.Valid {
		view.ClosedAt = &closedAt.Int64
	}
	if remaining := view.BudgetTokens - view.TokensUsed - view.TokensReserved; remaining > 0 {
		view.Remaining = remaining
	}

	// rowid is the tiebreak, not a second clock: two messages posted in the same second are
	// in insertion order, and SQLite's rowid is exactly that within one table.
	rows, err := db.QueryContext(ctx,
		`SELECT id, thread_id, author, body, tokens_charged, created_at
		   FROM messages WHERE thread_id=? ORDER BY created_at, rowid`, id)
	if err != nil {
		return view, err
	}
	defer rows.Close()
	view.Messages = []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ThreadID, &m.Author, &m.Body, &m.TokensCharged, &m.CreatedAt); err != nil {
			return view, err
		}
		view.Messages = append(view.Messages, m)
	}
	return view, rows.Err()
}

// PostMessageRequest is one turn. Tokens is what the poster actually used (input plus
// output); nil means it did not report, and the reservation is charged instead (D5).
type PostMessageRequest struct {
	Author string `json:"author"`
	Body   string `json:"body"`
	Tokens *int   `json:"tokens"`
}

// PostMessageResult is the post and the numbers that explain it: what was reserved, what
// was charged, and what the thread has left.
type PostMessageResult struct {
	Message   Message `json:"message"`
	Thread    Thread  `json:"thread"`
	Reserved  int     `json:"reserved"`
	Charged   int     `json:"charged"`
	Remaining int     `json:"remaining"`
}

// PostMessage reserves, decides, writes and settles one message (spec §13).
//
// All of it is one BEGIN IMMEDIATE transaction, and that is the point: the reservation
// check and the insert must see the same counters, or two posters each reserve the last of
// the budget and the thread ends up over it. The board does not hold a send in flight — the
// message text arrives already written — so the reservation is a decision, not an escrow,
// and tokens_reserved returns to zero in the same transaction that raised it. The column
// stays because §13's model and the schema include it, and a two-phase send would use it.
func PostMessage(ctx context.Context, db *sql.DB, clock Clock, threadID string, req PostMessageRequest) (PostMessageResult, error) {
	var out PostMessageResult
	author := strings.TrimSpace(req.Author)
	switch {
	case author == "":
		return out, fmt.Errorf("a message needs an author")
	case strings.TrimSpace(req.Body) == "":
		return out, fmt.Errorf("a message needs a body")
	}
	if req.Tokens != nil && *req.Tokens < 0 {
		return out, fmt.Errorf("tokens must not be negative")
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return out, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return out, fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var th Thread
	var owner sql.NullString
	var closedAt sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT id, COALESCE(work_item_id,''), subject, budget_tokens, max_output_tokens,
		        tokens_reserved, tokens_used, max_turns, turns_used, turn_owner, status,
		        created_at, closed_at
		   FROM threads WHERE id=?`, threadID).
		Scan(&th.ID, &th.WorkItemID, &th.Subject, &th.BudgetTokens, &th.MaxOutputTokens,
			&th.TokensReserved, &th.TokensUsed, &th.MaxTurns, &th.TurnsUsed, &owner,
			&th.Status, &th.CreatedAt, &closedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, fmt.Errorf("thread %s: %w", threadID, ErrNoThread)
		}
		return out, err
	}
	th.TurnOwner = owner.String
	if closedAt.Valid {
		th.ClosedAt = &closedAt.Int64
	}

	switch th.Status {
	case ThreadClosed:
		return out, fmt.Errorf("%w (%d of %d turns used)", ErrThreadClosed, th.TurnsUsed, th.MaxTurns)
	case ThreadFrozen:
		return out, fmt.Errorf("%w (raise its budget to continue)", ErrThreadFrozen)
	}

	// Alternation (D3): the second guard in §13, and the one that actually stops two
	// agents, because a cap only delays a loop. The operator is exempt: a human typing a
	// correction twice is not a loop.
	if th.TurnOwner != "" && author == th.TurnOwner && author != "admin" {
		return out, fmt.Errorf("%w: %q posted the last message", ErrSameAuthor, author)
	}

	if th.TurnsUsed >= th.MaxTurns {
		// Out of turns: the conversation is over, not frozen. Close it here so the next
		// reader sees why, and answer the poster with the same fact.
		if err := closeThread(ctx, conn, clock, th.ID, th.TurnsUsed); err != nil {
			return out, err
		}
		if err := commitConn(ctx, conn, &committed); err != nil {
			return out, err
		}
		return out, fmt.Errorf("%w (%d of %d turns used)", ErrThreadClosed, th.TurnsUsed, th.MaxTurns)
	}

	// estimated_prompt is the whole conversation the next model call would read, plus the
	// message being posted; max_output is the room the caller reserved to answer it.
	prompt, err := promptEstimate(ctx, conn, th.ID, req.Body)
	if err != nil {
		return out, err
	}
	reserve := prompt + th.MaxOutputTokens
	if th.TokensUsed+th.TokensReserved+reserve > th.BudgetTokens {
		// The first refusal of §13's list, and the only one that changes the thread's
		// state: a budget that cannot hold the next turn freezes the conversation.
		if err := freezeThread(ctx, conn, clock, th.ID, th.BudgetTokens, th.TokensUsed, reserve); err != nil {
			return out, err
		}
		if err := commitConn(ctx, conn, &committed); err != nil {
			return out, err
		}
		return out, fmt.Errorf("%w: reserve %d with %d used exceeds the %d budget",
			ErrThreadFrozen, reserve, th.TokensUsed, th.BudgetTokens)
	}

	// Settlement (D1, D5): the poster's count when it reported one, the reserve when it did
	// not. Not capped at the reserve — capping would make an under-reserving caller's
	// overspend free.
	charged := reserve
	if req.Tokens != nil {
		charged = *req.Tokens
	}
	msgID, err := NewToken()
	if err != nil {
		return out, err
	}
	now := clock.Now()
	msg := Message{
		ID: msgID, ThreadID: th.ID, Author: author, Body: req.Body,
		TokensCharged: charged, CreatedAt: now,
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO messages (id,thread_id,author,body,tokens_charged,created_at)
		 VALUES (?,?,?,?,?,?)`,
		msg.ID, msg.ThreadID, msg.Author, msg.Body, msg.TokensCharged, msg.CreatedAt); err != nil {
		return out, err
	}

	th.TokensUsed += charged
	th.TurnsUsed++
	th.TurnOwner = author
	status, closed := th.Status, th.ClosedAt
	if th.TurnsUsed >= th.MaxTurns {
		status = ThreadClosed
		closed = &now
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE threads SET tokens_used=?, turns_used=?, turn_owner=?, status=?, closed_at=?
		  WHERE id=?`,
		th.TokensUsed, th.TurnsUsed, th.TurnOwner, status, closed, th.ID); err != nil {
		return out, err
	}
	th.Status = status
	th.ClosedAt = closed
	if status == ThreadClosed {
		if err := AppendEvent(ctx, conn, clock, actorFor(author), "thread", th.ID, "closed",
			map[string]any{"reason": "turns used up", "turns_used": th.TurnsUsed}); err != nil {
			return out, err
		}
	}
	if err := commitConn(ctx, conn, &committed); err != nil {
		return out, err
	}

	out.Message = msg
	out.Thread = th
	out.Reserved = reserve
	out.Charged = charged
	if remaining := th.BudgetTokens - th.TokensUsed; remaining > 0 {
		out.Remaining = remaining
	}
	return out, nil
}

// promptEstimate is the whole conversation plus the message being posted, in the board's
// crude token estimate (D1). It reads every body, which is the cost the next model call
// would pay for context — the reason the budget is spent by conversation length and not by
// message count.
func promptEstimate(ctx context.Context, conn *sql.Conn, threadID, body string) (int, error) {
	rows, err := conn.QueryContext(ctx, `SELECT body FROM messages WHERE thread_id=? ORDER BY created_at, rowid`, threadID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	total := 0
	for rows.Next() {
		var prior string
		if err := rows.Scan(&prior); err != nil {
			return 0, err
		}
		total += estimateTokens(prior)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return total + estimateTokens(body), nil
}

// freezeThread is the one state change a refusal makes: the thread is frozen, the reason is
// in one event, and the transaction that carries it commits.
func freezeThread(ctx context.Context, conn *sql.Conn, clock Clock, id string, budget, used, reserve int) error {
	if _, err := conn.ExecContext(ctx, `UPDATE threads SET status=? WHERE id=?`, ThreadFrozen, id); err != nil {
		return err
	}
	return AppendEvent(ctx, conn, clock, "system", "thread", id, "frozen", map[string]any{
		"budget_tokens": budget, "tokens_used": used, "reserve": reserve,
	})
}

// closeThread closes a thread whose turns are used up.
func closeThread(ctx context.Context, conn *sql.Conn, clock Clock, id string, turnsUsed int) error {
	if _, err := conn.ExecContext(ctx,
		`UPDATE threads SET status=?, closed_at=? WHERE id=?`, ThreadClosed, clock.Now(), id); err != nil {
		return err
	}
	return AppendEvent(ctx, conn, clock, "system", "thread", id, "closed",
		map[string]any{"reason": "turns used up", "turns_used": turnsUsed})
}

// commitConn commits and records that it did, so the deferred rollback knows to stay quiet.
func commitConn(ctx context.Context, conn *sql.Conn, committed *bool) error {
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	*committed = true
	return nil
}

// actorFor turns a message author into an event actor. The board's actor vocabulary is
// "admin", "system" and node ids; a message author that is none of those is still that
// author, so it is passed through rather than guessed at.
func actorFor(author string) string { return author }

// PatchThreadRequest is the operator's lever: raise (or lower) a thread's budget. Nothing
// else about a conversation is editable — messages are append-only, and a closed thread
// stays closed (D8).
type PatchThreadRequest struct {
	BudgetTokens *int `json:"budget_tokens"`
}

// PatchThread changes the budget and, if the thread was frozen by a budget that no longer
// binds, reopens it.
//
// The reopen is not unconditional: it happens only when the thread's own numbers now fit,
// so PATCH cannot put a thread into a state its counters contradict. A patch that leaves a
// frozen thread unable to send is accepted (the operator may be working towards a number)
// and the thread stays frozen.
func PatchThread(ctx context.Context, db *sql.DB, clock Clock, id string, req PatchThreadRequest) (Thread, error) {
	if req.BudgetTokens == nil {
		return Thread{}, fmt.Errorf("nothing to change: send budget_tokens")
	}
	budget := *req.BudgetTokens
	if budget <= 0 {
		return Thread{}, fmt.Errorf("budget_tokens must be positive")
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return Thread{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return Thread{}, fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var th Thread
	var owner sql.NullString
	var closedAt sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT id, COALESCE(work_item_id,''), subject, budget_tokens, max_output_tokens,
		        tokens_reserved, tokens_used, max_turns, turns_used, turn_owner, status,
		        created_at, closed_at
		   FROM threads WHERE id=?`, id).
		Scan(&th.ID, &th.WorkItemID, &th.Subject, &th.BudgetTokens, &th.MaxOutputTokens,
			&th.TokensReserved, &th.TokensUsed, &th.MaxTurns, &th.TurnsUsed, &owner,
			&th.Status, &th.CreatedAt, &closedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Thread{}, fmt.Errorf("thread %s: %w", id, ErrNoThread)
		}
		return Thread{}, err
	}
	if th.Status == ThreadClosed {
		return Thread{}, fmt.Errorf("%w: a closed thread's budget is not a lever", ErrThreadClosed)
	}

	// Reopen only when the smallest possible next turn fits: max_output plus one prompt
	// token, which is the cheapest reservation PostMessage can make (estimateTokens("") is
	// 1). A looser test — "the budget is above what is used" — would set the thread open and
	// let the very next post freeze it again, which reads to an operator as a patch that did
	// not work.
	newStatus := th.Status
	if th.Status == ThreadFrozen && budget >= th.TokensUsed+th.MaxOutputTokens+1 {
		newStatus = ThreadOpen
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE threads SET budget_tokens=?, status=? WHERE id=?`, budget, newStatus, id); err != nil {
		return Thread{}, err
	}
	if newStatus != th.Status {
		if err := AppendEvent(ctx, conn, clock, "admin", "thread", id, newStatus, map[string]any{
			"budget_tokens": budget, "previous_budget": th.BudgetTokens,
		}); err != nil {
			return Thread{}, err
		}
	}
	if err := commitConn(ctx, conn, &committed); err != nil {
		return Thread{}, err
	}
	th.BudgetTokens = budget
	th.Status = newStatus
	return th, nil
}
