// internal/board/watches.go
package board

import "encoding/json"

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
