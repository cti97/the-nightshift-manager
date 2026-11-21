-- Migration 6 (M5 Task 1): threads, messages and checkpoints.
--
-- A thread is a conversation with a budget (spec §13). The reservation columns are on
-- the thread because the budget check and the post are one transaction: a reservation
-- that lives only in a process's memory would let two posters each see the full budget.
--
-- Two columns beyond the spec's table:
--   max_output_tokens   the second half of `reserve = estimated_prompt + max_output`.
--                       The spec's schema has nowhere to put it, and a constant in the
--                       code would be a budget nobody can see (M5 D2).
--   closed_at           when status became closed, so "when did this conversation end"
--                       is answerable without reading events
--
-- status is enforced here, not only in Go: frozen means the next reservation did not fit
-- (and stays frozen until an operator raises the budget), closed means the turns ran out.
CREATE TABLE threads (
  id                TEXT PRIMARY KEY,
  work_item_id      TEXT REFERENCES work_items(id),
  subject           TEXT NOT NULL,
  budget_tokens     INTEGER NOT NULL CHECK (budget_tokens > 0),
  max_output_tokens INTEGER NOT NULL DEFAULT 1024 CHECK (max_output_tokens > 0),
  tokens_reserved   INTEGER NOT NULL DEFAULT 0 CHECK (tokens_reserved >= 0),
  tokens_used       INTEGER NOT NULL DEFAULT 0 CHECK (tokens_used >= 0),
  max_turns         INTEGER NOT NULL DEFAULT 20 CHECK (max_turns > 0),
  turns_used        INTEGER NOT NULL DEFAULT 0 CHECK (turns_used >= 0),
  turn_owner        TEXT,
  status            TEXT NOT NULL CHECK (status IN ('open','frozen','closed')),
  created_at        INTEGER NOT NULL,
  closed_at         INTEGER
);

-- Messages are append-only (M5 D8): a conversation that can be rewritten is not an audit
-- trail. ids are random tokens, so they sort in no useful order and the index the thread
-- view reads through is (thread_id, created_at); within one second SQLite's rowid is the
-- insertion order, which is the order they were posted. (Same-second ties are why the
-- thread view must not ORDER BY created_at alone and call it a total order; it breaks ties
-- by rowid, which is what the index provides for free.)
CREATE TABLE messages (
  id             TEXT PRIMARY KEY,
  thread_id      TEXT NOT NULL REFERENCES threads(id),
  author         TEXT NOT NULL,
  body           TEXT NOT NULL,
  tokens_charged INTEGER NOT NULL DEFAULT 0 CHECK (tokens_charged >= 0),
  created_at     INTEGER NOT NULL
);
CREATE INDEX idx_messages_thread ON messages(thread_id, created_at);

-- Checkpoints are progress markers, not state migration (spec §12): "resume" means a new
-- attempt whose claim carries the latest one. seq is per run and monotonic, and the unique
-- index is what makes a retried write a refusal instead of a duplicate marker.
CREATE TABLE checkpoints (
  id                TEXT PRIMARY KEY,
  run_id            TEXT NOT NULL REFERENCES runs(id),
  seq               INTEGER NOT NULL CHECK (seq > 0),
  declared_progress TEXT NOT NULL,
  state             TEXT,
  artifacts         TEXT,
  created_at        INTEGER NOT NULL
);
CREATE UNIQUE INDEX idx_checkpoints_run_seq ON checkpoints(run_id, seq);
