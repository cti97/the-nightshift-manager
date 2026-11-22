-- Migration 8 (M8 Rule 2): a run says where its token counts came from.
--
-- The ledger cannot tell "this run used no tokens" from "nobody counted" — both arrive as 0 — and a
-- harness that reports nothing would therefore look free. A day of `claude -p` runs would show as
-- zero spend, which is worse than a ledger with a gap in it: it is a ledger that lies in the
-- direction that never stops work.
--
--   reported   the executor measured or was told (the built-in loop, or a harness writing NSM_USAGE_FILE)
--   estimated  the board's own estimate (1 + len/4), used where a count is impossible
--   unknown    nobody counted. The run still settles — the work was done — and spend/today counts it
--              as unmetered so the operator can see the gap rather than a zero.
ALTER TABLE runs ADD COLUMN token_source TEXT NOT NULL DEFAULT 'reported'
  CHECK (token_source IN ('reported','estimated','unknown'));
