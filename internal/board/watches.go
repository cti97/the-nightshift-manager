// internal/board/watches.go
package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
