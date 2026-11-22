-- Migration 7 (M7 Task 1): agents file work.
--
-- An item filed by an agent carries three facts a root item does not:
--
--   depth         how far below the operator's item it sits. Stored, not derived: a chain
--                 computed at read time can be laundered by a restart or a re-parent, and
--                 this is the column the depth limit is enforced against.
--   filed_by_run  which run asked for it. Fan-out is counted against a RUN, not an item: an
--                 item retried three times must not file fifteen children.
--   client_ref    the caller's own idempotency key. Agents retry; without this, every network
--                 hiccup files the same child twice and spends the work twice.
--
-- The unique index is partial because root items (and any item filed without a ref) have none,
-- and SQLite would otherwise treat every NULL as a duplicate of every other.
ALTER TABLE work_items ADD COLUMN depth INTEGER NOT NULL DEFAULT 0;
ALTER TABLE work_items ADD COLUMN client_ref TEXT;
ALTER TABLE work_items ADD COLUMN filed_by_run TEXT REFERENCES runs(id);
CREATE UNIQUE INDEX idx_work_items_client_ref ON work_items(client_ref) WHERE client_ref IS NOT NULL;
CREATE INDEX idx_work_items_filed_by_run ON work_items(filed_by_run);
-- The fan-out cap counts a parent's children on every filing, and parent_id had no index: the
-- count would scan the table an agent is about to write to.
CREATE INDEX idx_work_items_parent ON work_items(parent_id);
