-- Migration 2 (M2 Task 12): the tokens a run's budget charged for attempts that
-- reported no usage.
--
-- input_tokens and output_tokens are provider-reported actuals. A failed
-- Complete reports no usage (the seam permits an empty Usage only on failure),
-- and a retried run can be billed more than its final attempt's usage shows.
-- The executor therefore charges a failed attempt's reservation against the
-- run's budget and reports the sum here, separately from the measurements, so
-- Task 13's spend ledger counts what was actually charged.
ALTER TABLE runs ADD COLUMN reserved_tokens INTEGER NOT NULL DEFAULT 0;
