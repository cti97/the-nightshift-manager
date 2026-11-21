// internal/board/watches.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cti97/the-nightshift-manager/internal/caps"
)

// Watch is one standing trigger: a schedule, an optional observation, and the
// item it emits. It never holds work itself — firing creates an ordinary
// work_item and lets placement route it (spec §5, M4 D1).
type Watch struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Trigger  string        `json:"trigger"` // cron | interval | condition
	Schedule string        `json:"schedule"`
	Probe    string        `json:"probe,omitempty"`
	Template WatchTemplate `json:"template"`

	DedupeWindow  int    `json:"dedupe_window"`
	CooldownUntil *int64 `json:"cooldown_until,omitempty"`
	Enabled       bool   `json:"enabled"`

	LastFiredAt *int64 `json:"last_fired_at,omitempty"`
	NextFireAt  *int64 `json:"next_fire_at,omitempty"`
	// LastResult is the condition edge (D5): true, false, or nil when the watch
	// has never been observed. It is a column, not a field in memory, so a
	// manager restart cannot re-fire a condition that was already true.
	LastResult     *bool  `json:"last_result,omitempty"`
	LastSkipAt     *int64 `json:"last_skip_at,omitempty"`
	LastSkipReason string `json:"last_skip_reason,omitempty"`
	CreatedAt      int64  `json:"created_at"`
}

// WatchTemplate is what a fire turns into. Needs is parsed at create time, so a
// typo in a watch is refused when the operator types it rather than when the
// item is claimed at 03:00 (M4 Task 3).
type WatchTemplate struct {
	Title       string   `json:"title"`
	Brief       string   `json:"brief"`
	Needs       []string `json:"needs"`
	Priority    int      `json:"priority"`
	MaxAttempts int      `json:"max_attempts"`
}

// MarshalJSON writes an empty Needs list as [] rather than null, the same rule
// ApprovalRequest follows and for the same reason: the stored bytes of a
// template must not depend on whether the caller passed nil or an empty slice,
// and a reader ranging the list must not meet null.
func (t WatchTemplate) MarshalJSON() ([]byte, error) {
	type wire WatchTemplate
	w := wire(t)
	if len(w.Needs) == 0 {
		w.Needs = []string{}
	}
	return json.Marshal(w)
}

// CreateWatchRequest is the body of POST /admin/watches. DedupeWindow is a
// pointer because "absent" and "0" mean different things: absent takes the
// one-hour default, 0 explicitly disables dedupe for a watch whose schedule is
// already a sufficient floor.
type CreateWatchRequest struct {
	Name         string        `json:"name"`
	Trigger      string        `json:"trigger"`
	Schedule     string        `json:"schedule"`
	Probe        string        `json:"probe"`
	Template     WatchTemplate `json:"template"`
	DedupeWindow *int          `json:"dedupe_window"`
}

// defaultDedupeWindow is one hour, the schema's default and the plan's D6 floor.
const defaultDedupeWindow = 3600

// ErrWatchExists is a name collision, which the route answers 409: the board is
// fine, the caller's name is taken, and retrying the same body will never work.
var ErrWatchExists = errors.New("a watch with that name exists")

// CreateWatch validates everything a fire would otherwise discover at 03:00 —
// the trigger, the schedule, the probe's presence for a condition watch, and the
// template's title, brief and capabilities — computes the first fire, and stores
// the watch. Validation happens before any write, so a refused watch leaves no
// row and no event.
func CreateWatch(ctx context.Context, db *sql.DB, clock Clock, req CreateWatchRequest) (Watch, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return Watch{}, fmt.Errorf("a watch needs a name")
	}
	if req.Trigger != TriggerCron && req.Trigger != TriggerInterval && req.Trigger != TriggerCondition {
		return Watch{}, fmt.Errorf("trigger %q is not one of %q, %q, %q", req.Trigger, TriggerCron, TriggerInterval, TriggerCondition)
	}
	probe := strings.TrimSpace(req.Probe)
	if req.Trigger == TriggerCondition && probe == "" {
		// A condition watch is its probe: without one there is nothing to
		// observe, and every tick would be a skip with no explanation.
		return Watch{}, fmt.Errorf("a condition watch needs a probe: the command whose exit code decides whether it fires")
	}

	tmpl := req.Template
	tmpl.Title = strings.TrimSpace(tmpl.Title)
	tmpl.Brief = strings.TrimSpace(tmpl.Brief)
	if tmpl.Title == "" || tmpl.Brief == "" {
		return Watch{}, fmt.Errorf("the watch template needs a title and a brief: they become the item's")
	}
	needs, err := caps.Parse(tmpl.Needs)
	if err != nil {
		return Watch{}, fmt.Errorf("watch template needs: %w", err)
	}
	tmpl.Needs = needs
	if tmpl.MaxAttempts < 1 {
		tmpl.MaxAttempts = 3
	}
	if tmpl.Priority < 0 {
		return Watch{}, fmt.Errorf("watch template priority %d is negative", tmpl.Priority)
	}

	dedupe := defaultDedupeWindow
	if req.DedupeWindow != nil {
		if *req.DedupeWindow < 0 {
			return Watch{}, fmt.Errorf("dedupe_window %d is negative; 0 disables dedupe, a positive number is the floor in seconds", *req.DedupeWindow)
		}
		dedupe = *req.DedupeWindow
	}

	now := clock.Now()
	next, err := NextFire(req.Trigger, strings.TrimSpace(req.Schedule), now)
	if err != nil {
		return Watch{}, err
	}

	templateJSON, err := json.Marshal(tmpl)
	if err != nil {
		return Watch{}, fmt.Errorf("marshal watch template: %w", err)
	}
	id, err := NewToken()
	if err != nil {
		return Watch{}, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Watch{}, err
	}
	defer func() { _ = tx.Rollback() }() // a commit makes it a no-op

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO watches (id,name,trigger,schedule,probe,template,dedupe_window,enabled,next_fire_at,created_at)
		 VALUES (?,?,?,?,?,?,?,1,?,?)`,
		id, name, req.Trigger, strings.TrimSpace(req.Schedule), nullable(probe), string(templateJSON), dedupe, next, now); err != nil {
		// The UNIQUE(name) constraint is the only check a second writer can lose a
		// race on, and "constraint failed" is not an answer an operator can use.
		if strings.Contains(err.Error(), "UNIQUE") {
			return Watch{}, fmt.Errorf("%w: %q", ErrWatchExists, name)
		}
		return Watch{}, err
	}
	if err := AppendEvent(ctx, tx, clock, "admin", "watch", id, "created",
		map[string]any{"name": name, "trigger": req.Trigger, "schedule": req.Schedule, "next_fire_at": next}); err != nil {
		return Watch{}, err
	}
	if err := tx.Commit(); err != nil {
		return Watch{}, err
	}
	return Watch{
		ID: id, Name: name, Trigger: req.Trigger, Schedule: strings.TrimSpace(req.Schedule), Probe: probe,
		Template: tmpl, DedupeWindow: dedupe, Enabled: true, NextFireAt: &next, CreatedAt: now,
	}, nil
}

// watchColumns is the one column list every read uses, so a new column cannot be
// added to one query and forgotten in the other.
const watchColumns = `id,name,trigger,COALESCE(schedule,''),COALESCE(probe,''),template,
	dedupe_window,cooldown_until,enabled,last_fired_at,next_fire_at,last_result,
	last_skip_at,COALESCE(last_skip_reason,''),created_at`

// scanWatch reads one row of watchColumns.
func scanWatch(sc interface{ Scan(...any) error }) (Watch, error) {
	var w Watch
	var rawTemplate string
	var enabled int
	var cooldown, lastFired, nextFire, lastResult, lastSkip sql.NullInt64
	if err := sc.Scan(&w.ID, &w.Name, &w.Trigger, &w.Schedule, &w.Probe, &rawTemplate,
		&w.DedupeWindow, &cooldown, &enabled, &lastFired, &nextFire, &lastResult,
		&lastSkip, &w.LastSkipReason, &w.CreatedAt); err != nil {
		return Watch{}, err
	}
	if err := json.Unmarshal([]byte(rawTemplate), &w.Template); err != nil {
		return Watch{}, fmt.Errorf("watch %s template: %w", w.ID, err)
	}
	w.Enabled = enabled == 1
	w.CooldownUntil = int64Ptr(cooldown)
	w.LastFiredAt = int64Ptr(lastFired)
	w.NextFireAt = int64Ptr(nextFire)
	w.LastSkipAt = int64Ptr(lastSkip)
	if lastResult.Valid {
		v := lastResult.Int64 == 1
		w.LastResult = &v
	}
	return w, nil
}

func int64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

// ListWatches returns every watch, oldest first. An empty board returns an empty
// slice, not nil: the route answers [] so a caller can tell "no watches" from "the
// request failed" (the same rule as the approvals queue).
func ListWatches(ctx context.Context, db *sql.DB) ([]Watch, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+watchColumns+` FROM watches ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Watch{}
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetWatch returns one watch. sql.ErrNoRows stays wrapped, so the route can answer
// 404 for a wrong id and 500 for a board it cannot read.
func GetWatch(ctx context.Context, db *sql.DB, id string) (Watch, error) {
	w, err := scanWatch(db.QueryRowContext(ctx, `SELECT `+watchColumns+` FROM watches WHERE id=?`, id))
	if err != nil {
		return Watch{}, fmt.Errorf("watch %s: %w", id, err)
	}
	return w, nil
}

// PatchWatchRequest is the body of PATCH /admin/watches/{id}: the operator's two
// levers, and only these two. Both are pointers because omitting a field must mean
// "leave it alone" — that is what makes "clear the hold" expressible as
// cooldown_until: 0 rather than as an absent field.
type PatchWatchRequest struct {
	Enabled       *bool  `json:"enabled"`
	CooldownUntil *int64 `json:"cooldown_until"`
}

// PatchWatch disables, enables or holds a watch (M4 D6, D10). There is no delete:
// disabling keeps the row and its history.
//
// Enabling re-anchors the schedule from now instead of letting a stale next_fire_at
// fire at once. The two cases are different: a manager that was down leaves the
// watch due and D2's one-catch-up rule applies, but an operator who disabled the
// watch said "not now" for that window, and re-enabling it is the start of a new
// schedule rather than a debt to be paid.
func PatchWatch(ctx context.Context, db *sql.DB, clock Clock, id string, req PatchWatchRequest) (Watch, error) {
	if req.Enabled == nil && req.CooldownUntil == nil {
		return Watch{}, fmt.Errorf("nothing to change: send enabled, cooldown_until, or both")
	}
	if req.CooldownUntil != nil && *req.CooldownUntil < 0 {
		return Watch{}, fmt.Errorf("cooldown_until %d is negative; 0 clears the hold", *req.CooldownUntil)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return Watch{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var current Watch
	current, err = scanWatch(tx.QueryRowContext(ctx, `SELECT `+watchColumns+` FROM watches WHERE id=?`, id))
	if err != nil {
		return Watch{}, fmt.Errorf("watch %s: %w", id, err)
	}

	now := clock.Now()
	sets := make([]string, 0, 3)
	args := make([]any, 0, 4)
	payload := map[string]any{}

	if req.Enabled != nil && *req.Enabled != current.Enabled {
		sets = append(sets, "enabled=?")
		args = append(args, boolInt(*req.Enabled))
		payload["enabled"] = *req.Enabled
		if *req.Enabled {
			next, err := NextFire(current.Trigger, current.Schedule, now)
			if err != nil {
				return Watch{}, err
			}
			sets = append(sets, "next_fire_at=?")
			args = append(args, next)
			payload["next_fire_at"] = next
		}
	}
	if req.CooldownUntil != nil {
		sets = append(sets, "cooldown_until=?")
		if *req.CooldownUntil == 0 {
			args = append(args, nil)
			payload["cooldown_until"] = nil
		} else {
			args = append(args, *req.CooldownUntil)
			payload["cooldown_until"] = *req.CooldownUntil
		}
	}
	if len(sets) == 0 {
		// Nothing changed: an idempotent patch is a success, not an error, and it
		// must not write an event that says something happened.
		return current, nil
	}

	args = append(args, id)
	res, err := tx.ExecContext(ctx, `UPDATE watches SET `+strings.Join(sets, ", ")+` WHERE id=?`, args...)
	if err != nil {
		return Watch{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Watch{}, err
	}
	if n != 1 {
		return Watch{}, fmt.Errorf("watch %s disappeared while it was being changed", id)
	}
	if err := AppendEvent(ctx, tx, clock, "admin", "watch", id, "updated", payload); err != nil {
		return Watch{}, err
	}
	if err := tx.Commit(); err != nil {
		return Watch{}, err
	}
	return GetWatch(ctx, db, id)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// FireResult is what one evaluation did. Fired is false for a skipped watch, and
// Skipped says which check refused it — the reason a scheduler's log line and the
// watch's own last_skip_reason both quote.
type FireResult struct {
	Fired   bool   `json:"fired"`
	ItemID  string `json:"item_id,omitempty"`
	Skipped string `json:"skipped,omitempty"`
}

// Fire evaluates one due watch and, if it fires, emits its item — in one
// transaction (M4 D1). The item, the schedule advance and the event are written
// together, because a crash between any two either double-fires the watch or loses
// the fire, and both are invisible until someone counts items.
//
// observedNextFire is the value the scan saw. The advance is guarded on it, so two
// schedulers that scanned the same row cannot both fire: the first advances
// next_fire_at, the second's UPDATE matches nothing and its transaction rolls back.
// That is the same single-writer rule Claim uses, applied to a much smaller race.
//
// The item is queued and ordinary in every other respect: placement routes it, a
// node claims it, it can escalate and be approved. A watch emits work, it does not
// run it.
// probeResult is one probe's answer, carried from the unlocked phase into the
// transaction. By the time the transaction decides, the probe is finished: this is a
// value, not a handle.
type probeResult struct {
	exit     int
	tail     string
	timedOut bool
}

func Fire(ctx context.Context, db *sql.DB, clock Clock, watchID string, observedNextFire int64) (FireResult, error) {
	// Phase 1, unlocked: decide whether this watch is worth a probe, and if it is, run
	// it WITHOUT holding a write lock.
	//
	// The probe must not run inside the transaction. BEGIN IMMEDIATE takes the
	// database's single write lock, and a probe may take thirty seconds (watchProbeTimeout):
	// one slow probe would stop every claim, heartbeat and report on the board for as
	// long as it ran, and the board's whole promise is that it keeps working while a
	// run is stuck. An observation is a fact about a moment, so it is taken here and
	// used below only if the row is still the same row.
	w, err := GetWatch(ctx, db, watchID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// A watch that vanished between the scan and this evaluation is not a
			// failure of the pass: there is simply nothing to evaluate. (There is no
			// delete route — D10 — so this is a hand-edited board, and reporting it as
			// an error would make one bad row stop the scheduler every tick.)
			return FireResult{Skipped: "the watch no longer exists"}, nil
		}
		return FireResult{}, fmt.Errorf("watch %s: %w", watchID, err)
	}
	if !w.Enabled {
		// Not an evaluation: the row is off, and D7's skip record is for a watch that
		// was considered and refused. Writing here would also overwrite the schedule a
		// re-enable is about to re-anchor.
		return FireResult{Skipped: "the watch is disabled"}, nil
	}
	if w.NextFireAt == nil || *w.NextFireAt != observedNextFire {
		// The schedule moved between the scan and this evaluation: another scheduler
		// fired it, or an operator patched it. Either way this scan is stale and must
		// not spend a probe on it.
		return FireResult{Skipped: "the schedule moved since the scan"}, nil
	}
	var probe *probeResult
	if w.Probe != "" && skipReason(w, clock.Now()) == "" {
		// A held or windowed watch does not pay for a probe (D6's order: the decision
		// is already made).
		exit, tail, timedOut := runWatchProbe(ctx, w.Probe, watchProbeTimeout)
		probe = &probeResult{exit: exit, tail: tail, timedOut: timedOut}
	}

	if ctx.Err() != nil {
		// Shutting down between the probe and the transaction: the probe's death is ours,
		// not the watch's, so nothing is written and the slot stays due.
		return FireResult{Skipped: "the evaluation was cancelled"}, nil
	}

	// Phase 2, the transaction. Everything read in phase 1 is re-read here, because a
	// probe can take half a minute and an operator can hold, disable or patch the
	// watch while it runs. BEGIN IMMEDIATE on a dedicated connection, the pattern
	// Claim uses and for the same reason: a deferred transaction that reads and then
	// writes can lose the upgrade to another writer with SQLITE_BUSY no matter how
	// long its busy timeout is.
	conn, err := db.Conn(ctx)
	if err != nil {
		return FireResult{}, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return FireResult{}, fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	w, err = scanWatch(conn.QueryRowContext(ctx, `SELECT `+watchColumns+` FROM watches WHERE id=?`, watchID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return FireResult{Skipped: "the watch no longer exists"}, nil
		}
		return FireResult{}, fmt.Errorf("watch %s: %w", watchID, err)
	}
	if !w.Enabled {
		return FireResult{Skipped: "the watch is disabled"}, nil
	}
	if w.NextFireAt == nil || *w.NextFireAt != observedNextFire {
		return FireResult{Skipped: "the schedule moved since the scan"}, nil
	}

	now := clock.Now()
	next, err := NextFire(w.Trigger, w.Schedule, now)
	if err != nil {
		return FireResult{}, fmt.Errorf("watch %s: %w", watchID, err)
	}

	// skip consumes a slot the watch did not fire (D7): the schedule advances and the
	// reason is written on the row. No event — ten held slots would otherwise be ten
	// entries saying the same thing, and the row already carries the answer.
	//
	// result is what the probe observed, when one ran: a refused fire still records
	// the observation (it is why the condition's edge is where it is), while a hold
	// records nothing because nothing was observed.
	skip := func(reason string, result *bool) (FireResult, error) {
		res, err := conn.ExecContext(ctx,
			`UPDATE watches SET next_fire_at=?, last_skip_at=?, last_skip_reason=?,
			        last_result=COALESCE(?, last_result)
			  WHERE id=? AND enabled=1 AND next_fire_at <= ?`,
			next, now, reason, boolOrNil(result), watchID, now)
		if err != nil {
			return FireResult{}, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return FireResult{}, err
		}
		if n != 1 {
			return FireResult{Skipped: "the slot was already fired, or the watch is not due"}, nil
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return FireResult{}, err
		}
		committed = true
		return FireResult{Skipped: reason}, nil
	}

	// The operator's hold, then the system's window (D6) — re-checked, because the
	// operator may have held the watch while the probe ran.
	if reason := skipReason(w, now); reason != "" {
		return skip(reason, nil)
	}

	// The probe (D5, D8). A condition watch fires on the false→true transition and
	// only there; last_result holds the previous observation, so a manager restart
	// cannot re-fire a condition that was already true. An interval or cron watch uses
	// its probe as a gate: true proceeds to the fire, false is a skip.
	var observed *bool
	if w.Probe != "" {
		if probe == nil {
			// Phase 1 saw a refusal (a hold or a window) and did not probe; the refusal
			// is gone now, so this evaluation has no observation. Firing a gated watch on
			// an observation nobody took would be a guess; leave the slot where it is and
			// let the next pass evaluate it properly.
			return FireResult{Skipped: "the watch became firable while it was being evaluated; the next pass will take it"}, nil
		}
		if ctx.Err() != nil {
			// The manager is shutting down, and the probe it killed is why there is no
			// observation. Recording that as the watch's skip would put our shutdown in
			// the watch's history and, for a condition, look like a failed check. The slot
			// stays due for whoever starts next.
			return FireResult{Skipped: "the evaluation was cancelled"}, nil
		}
		if probe.timedOut {
			// The observation did not happen, so nothing is recorded: treating a timeout
			// as "false" would make a condition fire spuriously on the tick after a slow
			// probe, and treat an outage as a state change.
			return skip(fmt.Sprintf("the probe did not finish within %s; the observation did not happen", watchProbeTimeout), nil)
		}
		v := probe.exit == 0
		observed = &v
		if w.Trigger == TriggerCondition {
			if !v {
				return skip(probeReason("the condition is false", probe.exit, probe.tail), observed)
			}
			if w.LastResult != nil && *w.LastResult {
				return skip("the condition is already true; a condition fires on the transition, not on every check", observed)
			}
		} else if !v {
			return skip(probeReason("the probe reported false", probe.exit, probe.tail), observed)
		}
	}

	// Claim the slot before doing any work. The token check is the re-read above
	// (a fire always advances next_fire_at, so a second caller holding the old token
	// is refused there, with a message that says which refusal it was); BEGIN
	// IMMEDIATE is what makes that check atomic against another manager process.
	//
	// The SQL guards below are the rules the database can state for itself: the watch
	// is still enabled, and it is actually due. The due clause is not decoration —
	// Fire does not trust its scanner, and a scheduler that called it for every watch
	// would otherwise emit every item early.
	//
	// A `next_fire_at = ?` clause and a `last_fired_at < now` clause were both
	// removed rather than kept: with the serialized writer, the in-transaction
	// re-read, and these two guards, neither could be reached. An unreachable guard
	// is a mutation nobody can catch, which is worse than no guard.
	res, err := conn.ExecContext(ctx,
		`UPDATE watches SET last_fired_at=?, next_fire_at=?, last_result=COALESCE(?, last_result)
		  WHERE id=? AND enabled=1 AND next_fire_at <= ?`,
		now, next, boolOrNil(observed), watchID, now)
	if err != nil {
		return FireResult{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return FireResult{}, err
	}
	if n != 1 {
		return FireResult{Skipped: "the slot was already fired, or the watch is not due"}, nil
	}

	itemID, err := NewToken()
	if err != nil {
		return FireResult{}, err
	}
	needsJSON, err := json.Marshal(nonNilList(w.Template.Needs))
	if err != nil {
		return FireResult{}, err
	}
	maxAttempts := w.Template.MaxAttempts
	if maxAttempts < 1 {
		// A hand-edited row can carry 0; CreateItem would default it, but this
		// insert is direct and an item with no attempts can never succeed.
		maxAttempts = 3
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO work_items (id,title,brief,needs,status,priority,max_attempts,created_by,created_at,updated_at)
		 VALUES (?,?,?,?,'queued',?,?,?,?,?)`,
		itemID, w.Template.Title, w.Template.Brief, string(needsJSON),
		w.Template.Priority, maxAttempts, "watch:"+w.ID, now, now); err != nil {
		return FireResult{}, err
	}

	// The event names the item, the trigger and the schedule: "a watch fired" is not
	// an audit trail, "this watch emitted this item at this slot" is.
	if err := AppendEvent(ctx, conn, clock, "watch:"+w.ID, "watch", w.ID, "fired",
		map[string]any{
			"item": itemID, "trigger": w.Trigger, "schedule": w.Schedule,
			"next_fire_at": next, "observed": observed,
		}); err != nil {
		return FireResult{}, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return FireResult{}, err
	}
	committed = true
	return FireResult{Fired: true, ItemID: itemID}, nil
}

// skipReason is the first refusal that still consumes the slot (M4 D6): the
// operator's hold, then the system's dedupe floor. Empty means the watch may fire.
//
// The order is part of the contract: "held by the operator" must win over
// "deduped", or an operator who deliberately held a watch reads a reason about a
// window they did not set and goes looking for the wrong thing.
//
// A hold and a window are different mechanisms with different owners (D6): the
// window is the system's floor between emissions and is measured from the last
// fire; the hold is a timestamp an operator set, and firing never clears it.
func skipReason(w Watch, now int64) string {
	if w.CooldownUntil != nil && *w.CooldownUntil > now {
		return fmt.Sprintf("held by the operator until %s",
			time.Unix(*w.CooldownUntil, 0).UTC().Format(time.RFC3339))
	}
	if w.DedupeWindow > 0 && w.LastFiredAt != nil {
		// The window ends at last_fired + window; firing exactly at that second is
		// allowed, because the window is "at least this much time between emissions",
		// not "never at this instant".
		if until := *w.LastFiredAt + int64(w.DedupeWindow); until > now {
			return fmt.Sprintf("inside the %s dedupe window, which ends at %s",
				time.Duration(w.DedupeWindow)*time.Second,
				time.Unix(until, 0).UTC().Format(time.RFC3339))
		}
	}
	return ""
}

// boolOrNil renders an optional observation as the value a COALESCE can use: nil
// leaves the stored result alone, 1/0 records it.
func boolOrNil(v *bool) any {
	if v == nil {
		return nil
	}
	if *v {
		return 1
	}
	return 0
}

// probeReason is a skip reason that carries the probe's own evidence: its exit code
// and the tail of what it printed. The tail is bounded in runes so the message stays
// readable and stays valid UTF-8.
func probeReason(prefix string, exit int, tail string) string {
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return fmt.Sprintf("%s (probe exit %d)", prefix, exit)
	}
	runes := []rune(tail)
	if len(runes) > 200 {
		tail = string(runes[:200]) + "…"
	}
	return fmt.Sprintf("%s (probe exit %d: %s)", prefix, exit, tail)
}

// WatchPass counts what one scheduler pass did. Failed is a watch whose evaluation
// returned an error (a row the board cannot read, a database failure); those are also
// returned as the pass's error, because a count alone does not tell an operator which
// row to look at.
type WatchPass struct {
	Fired   int `json:"fired"`
	Skipped int `json:"skipped"`
	Failed  int `json:"failed"`
}

// dueWatch is the scan's row: an id and the token it was due at. Nothing else is
// read here — every decision is taken on rows re-read inside Fire's transaction,
// because anything read in the scan can be stale by the time the write happens (the
// reaper's rule, applied again).
type dueWatch struct {
	id       string
	observed int64
}

// watchPassBudget bounds how long one pass may spend starting watches. A probe can
// take thirty seconds, so a board with many due probes could otherwise spend ten
// minutes in a single pass while every other watch stays unexamined. A watch the budget
// cuts off is still due — the next pass picks it up, in the same order.
//
// The budget is wall-clock, deliberately: it is an operational bound on a subprocess
// tree, not a fact about board time, and a fake clock must not be able to turn it off.
var watchPassBudget = 20 * time.Second

// RunWatchesOnce is one scheduler pass: scan the due watches and fire each one.
//
// A watch that errors does not stop the pass. The scan order is stable, so returning
// at the first failure would starve every watch behind it forever — the same reason
// the reaper collects its errors and keeps going.
func RunWatchesOnce(ctx context.Context, db *sql.DB, clock Clock) (WatchPass, error) {
	now := clock.Now()
	rows, err := db.QueryContext(ctx,
		`SELECT id, next_fire_at FROM watches
		  WHERE enabled=1 AND next_fire_at IS NOT NULL AND next_fire_at <= ?
		  ORDER BY next_fire_at, id`, now)
	if err != nil {
		return WatchPass{}, err
	}
	var due []dueWatch
	for rows.Next() {
		var d dueWatch
		if err := rows.Scan(&d.id, &d.observed); err != nil {
			rows.Close()
			return WatchPass{}, err
		}
		due = append(due, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return WatchPass{}, err
	}

	var pass WatchPass
	var errs []error
	started := time.Now()
	for _, d := range due {
		if time.Since(started) > watchPassBudget {
			// Out of time for this pass. Everything left is still due; the next pass
			// starts with the same ordering, so the oldest slots go first.
			break
		}
		if err := ctx.Err(); err != nil {
			// Shutdown mid-pass: the remaining watches stay due and the next
			// manager picks them up. Reporting it as a failure would blame the rows.
			break
		}
		res, err := Fire(ctx, db, clock, d.id, d.observed)
		if err != nil {
			if ctx.Err() != nil {
				// The manager is stopping; the error is the shutdown, not the watch.
				break
			}
			pass.Failed++
			errs = append(errs, fmt.Errorf("watch %s: %w", d.id, err))
			continue
		}
		if res.Fired {
			pass.Fired++
		} else {
			pass.Skipped++
		}
	}
	return pass, errors.Join(errs...)
}

// RunWatches ticks forever; cancel ctx to stop it. It runs on the manager, next to
// the reaper: the reaper is about leases (30s) and this is about minutes, so the two
// cadences are separate constants rather than one shared tick.
//
// A pass that did nothing logs nothing: a watch whose schedule is quiet is not news,
// and a line per tick would bury the one pass that mattered. The watch list's
// overdue state is what tells an operator the loop has stopped.
func (s *Server) RunWatches(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pass, err := RunWatchesOnce(ctx, s.db, s.clock)
			if err != nil {
				s.logf("watches: %v", err)
			}
			// Skips are not logged. A healthy condition watch is a skip on every tick
			// (the condition is true and it fires on the transition), and a held watch is
			// a skip on every slot: a line per tick would bury the pass that mattered.
			// The reason is on the row (last_skip_at, last_skip_reason) and in the state
			// sentence, which is where an operator looks for it.
			if pass.Fired > 0 || pass.Failed > 0 {
				s.logf("watches: fired %d, failed %d", pass.Fired, pass.Failed)
			}
		}
	}
}

// watchStaleAfter is how late a slot may be before the board stops calling it
// "the next pass will take it" and starts calling the scheduler stopped. It is a
// grace, not a threshold of interest: the manager's tick is 15s, so a slot a few
// seconds old is a watch that is about to fire, and calling that "the scheduler is
// not running" on every watch for fifteen seconds per slot would make the sentence
// worthless. Two minutes is eight ticks — long enough that a healthy manager never
// trips it, short enough that a stopped one is noticed within the minute.
const watchStaleAfter = int64(2 * time.Minute / time.Second)

// WatchView is a watch plus the sentence an operator reads instead of the columns
// (M4 D10). It is derived on read and stored nowhere: a state column would need
// writing on every tick, would be wrong between ticks, and would disagree with the
// scheduling columns the way a cache does.
type WatchView struct {
	Watch
	State string `json:"state"`
}

// ViewWatch pairs a stored row with its sentence.
func ViewWatch(w Watch, now int64) WatchView {
	return WatchView{Watch: w, State: WatchState(w, now)}
}

// WatchState answers "why is this watch quiet" from what is stored (M4 Task 8). The
// order is the fix order of Fire's own gate chain, so the sentence names the same
// refusal the scheduler would: disabled, held, then — for a schedule — overdue or
// waiting, with a current skip appended.
//
// It is a read. Nothing here writes, for the /why reason: asking why must not change
// the answer it is explaining.
func WatchState(w Watch, now int64) string {
	name := w.Name
	if name == "" {
		name = w.ID
	}
	if !w.Enabled {
		// First because it is the first thing Fire checks, and because a disabled
		// watch's other columns are frozen: reporting a hold or an overdue slot
		// would describe a schedule that is not being consulted at all.
		return fmt.Sprintf("watch %q is disabled; it will not fire until it is switched back on", name)
	}
	if w.CooldownUntil != nil && *w.CooldownUntil > now {
		return fmt.Sprintf("watch %q is held by the operator until %s (%s from now)",
			name, isoTime(*w.CooldownUntil), waitedFor(*w.CooldownUntil-now))
	}
	if w.Trigger == TriggerCondition {
		return conditionState(w, name, now)
	}
	return scheduleState(w, name, now)
}

// conditionState describes a condition watch. Its next_fire_at is always "now" by
// design — it is evaluated every pass — so the overdue rule must not be applied to
// it: a perfectly healthy condition watch would read as a stopped scheduler.
//
// The last observation's age is what answers the scheduler question here, and it is
// knowable because every evaluation writes a skip or a fire (Task 5's decision to
// record the "already true" skip is what makes this sentence possible).
func conditionState(w Watch, name string, now int64) string {
	checkedAt := w.LastSkipAt
	if w.LastFiredAt != nil && (checkedAt == nil || *w.LastFiredAt > *checkedAt) {
		checkedAt = w.LastFiredAt
	}
	if checkedAt == nil {
		return fmt.Sprintf("watch %q has not been checked yet; its probe runs on the scheduler's next pass", name)
	}
	observation := "the probe's last run produced no observation (it timed out or could not start)"
	if w.LastResult != nil {
		if *w.LastResult {
			observation = "the condition is true"
		} else {
			observation = "the condition is false"
		}
	}
	sentence := fmt.Sprintf("watch %q is a condition watch: %s (last checked %s ago)",
		name, observation, waitedFor(now-*checkedAt))
	if age := now - *checkedAt; age > watchStaleAfter {
		sentence += fmt.Sprintf("; nothing has looked at it for longer than %s, so the scheduler is not running or is stuck", waitedFor(watchStaleAfter))
	}
	// A condition's every quiet evaluation is a skip, so its reason is usually just
	// the observation already stated above. It is worth printing only when the
	// observation is missing — that is the case no other clause covers.
	if w.LastResult == nil && w.LastSkipReason != "" && w.LastSkipAt != nil {
		sentence += "; it last skipped at " + isoTime(*w.LastSkipAt) + " because " + w.LastSkipReason
	}
	return sentence
}

// scheduleState describes a triggered watch by its next slot: future means waiting,
// past beyond the grace means the scheduler is not running, past inside the grace
// means the next pass will take it.
func scheduleState(w Watch, name string, now int64) string {
	switch {
	case w.NextFireAt == nil:
		// Not reachable through CreateWatch, which always computes a slot; a
		// hand-edited row is the case, and "no slot" is the honest answer.
		return fmt.Sprintf("watch %q has no next slot recorded, so it will not fire; set its schedule again", name)
	case *w.NextFireAt > now:
		if w.LastFiredAt == nil {
			return fmt.Sprintf("watch %q has not fired yet; its first slot is at %s (%s away)",
				name, isoTime(*w.NextFireAt), waitedFor(*w.NextFireAt-now))
		}
		return fmt.Sprintf("watch %q is waiting for its next slot at %s (%s away)%s",
			name, isoTime(*w.NextFireAt), waitedFor(*w.NextFireAt-now), skipClause(w))
	case now-*w.NextFireAt > watchStaleAfter:
		// The sentence that matters: a watch whose row still says it was due ten
		// minutes ago is not waiting for anything. Either the manager is down or the
		// scheduler loop is not running, and saying "waiting" would send the operator
		// looking at the schedule instead of at the process.
		return fmt.Sprintf("watch %q is overdue: it was due at %s (%s ago) and has not fired, so the scheduler is not running or is stuck",
			name, isoTime(*w.NextFireAt), waitedFor(now-*w.NextFireAt))
	default:
		return fmt.Sprintf("watch %q is due at %s; the scheduler's next pass will take it%s",
			name, isoTime(*w.NextFireAt), skipClause(w))
	}
}

// skipClause is the tail that says why the last slot did not fire, and only when
// that skip is current. A skip older than the last fire describes a slot that has
// since been overtaken — printing it would tell an operator about a refusal that no
// longer applies, which is the same failure as a stale cache with worse grammar.
func skipClause(w Watch) string {
	if w.LastSkipAt == nil || w.LastSkipReason == "" {
		return ""
	}
	if w.LastFiredAt != nil && *w.LastSkipAt <= *w.LastFiredAt {
		return ""
	}
	return "; it last skipped at " + isoTime(*w.LastSkipAt) + " because " + w.LastSkipReason
}

// isoTime renders a stored second for a sentence. UTC and RFC 3339, like the skip
// reasons Task 5 writes, so a reader comparing the two is comparing one format.
func isoTime(ts int64) string {
	return time.Unix(ts, 0).UTC().Format(time.RFC3339)
}
