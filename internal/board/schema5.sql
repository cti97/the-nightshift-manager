-- Migration 5 (M4 Task 1): the watches table.
--
-- A watch is a standing trigger, not work: firing one creates an ordinary
-- work_item and placement routes it (spec §5). It never holds a lease and never
-- runs a model.
--
-- Four columns beyond the spec's table, each with a reason M4's decisions give:
--   last_result      the condition edge, stored in the row so a manager restart
--                    cannot re-fire a condition that was already true (D5)
--   last_skip_at     when the scheduler last considered this watch and did not
--   last_skip_reason fire, and why (D7). A watch that goes quiet must be
--                    explainable from the board, not from the manager's log.
--   created_at       every other table has one
--
-- template is JSON: {title, brief, needs, priority, max_attempts}.
CREATE TABLE watches (
  id               TEXT PRIMARY KEY,
  name             TEXT NOT NULL UNIQUE,
  trigger          TEXT NOT NULL CHECK (trigger IN ('cron','interval','condition')),
  schedule         TEXT,
  probe            TEXT,
  template         TEXT NOT NULL,
  dedupe_window    INTEGER NOT NULL DEFAULT 3600,
  cooldown_until   INTEGER,
  enabled          INTEGER NOT NULL DEFAULT 1,
  last_fired_at    INTEGER,
  next_fire_at     INTEGER,
  last_result      INTEGER,
  last_skip_at     INTEGER,
  last_skip_reason TEXT,
  created_at       INTEGER NOT NULL
);

-- The scheduler's scan is exactly this predicate and nothing else (D2): due,
-- enabled watches, in the index's own order. A partial index keeps the disabled
-- rows out of the way they would otherwise occupy.
CREATE INDEX idx_watches_due ON watches(next_fire_at) WHERE enabled = 1;
