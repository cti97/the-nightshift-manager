-- Migration 4 (M3 Task 1): the approval request a parked item carries, and the
-- index the approvals page lists through.
--
-- An item in 'needs_approval' must tell a human what is being asked, why, and
-- what the choices are. D1 stores that on the item instead of replaying the
-- work item's events: the approvals page is then one query, rather than a
-- replay nobody writes twice the same way. The column is nullable -- an item
-- that is not parked has no request -- and holds JSON because a request is
-- read whole and never queried by field.
ALTER TABLE work_items ADD COLUMN approval_request TEXT;

-- The approvals page reads `WHERE status = 'needs_approval' ORDER BY created_at`.
-- Partial, because only parked items are ever read this way: the index stays
-- the size of the parked set rather than the whole board. created_at is the
-- second key so that ORDER BY is the index's own order and needs no sort.
-- status is in the key too: every parked row has the same value there, so it
-- excludes nothing, but it makes the index and its predicate legible together
-- in pragma_index_info.
CREATE INDEX idx_items_parked ON work_items(status, created_at) WHERE status = 'needs_approval';
