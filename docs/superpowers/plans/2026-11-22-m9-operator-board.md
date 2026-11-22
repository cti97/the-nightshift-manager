# M9 — the operator's board

Spec §14 (dashboard), §20 (operations), §22 (roadmap).

Two gaps that only appear once the system works: nothing ever deleted anything, and there was no page
that showed the board. Both are about a board that has been running for a year, which is the only
board that matters.

## T1 — retention (done)

`events` is append-only and held a row for every transition since the board was created. The file is
copied whole by the snapshot and replica paths every minute, so its growth is paid for repeatedly.

`PruneEvents` deletes the oldest events in batches of 2000, on the reaper's clock, with
`--retention` defaulting to 90 days (`0` keeps everything). Three decisions:

- **runs are never pruned.** They are the ledger's only source and `work_items.filed_by_run`
  references them; rewriting that history to save disk is a worse trade than the disk.
- **the floor is 24 hours**, and a smaller value is raised rather than refused: a typo in a unit file
  must not be able to delete today's spine at the next tick.
- **the boundary is exclusive at the start**: an event exactly at the cutoff is kept. Same class of
  decision as the watch dedupe window.

`POST /admin/retention/prune {keep_hours}` is "prune now" — an operator who has just changed the
setting, or who is about to copy the database, should not wait for the tick.

Acceptance: `TestRetentionPrunesOldEventsAndKeepsRuns`, `TestRetentionKeepsTheFloor`,
`TestZeroRetentionKeepsEverything`, `TestRetentionPrunesInBatches`,
`TestTheCutoffIsExclusiveAtTheBoundary`, `TestThePruneRouteIsAnOperatorsTool`.

## T2 — the board page (done)

`GET /admin`: every item grouped by status — waiting-on-a-human first — each linking into the item
tree, with today's spend, the unmetered count and a pause banner. HTML to a browser, JSON to a caller.

The `ORDER BY` is a contract rather than a preference: it decides which items survive truncation
(200), and the row a full page must never drop is the one waiting on a human. That is what
`TestTheParkedItemSurvivesTruncation` exists for; it needed distinct timestamps before it could fail.

Acceptance: `TestTheBoardPageShowsEveryItemWithAWayIn`, `TestTheBoardAnswersJSONToACaller`,
`TestAnEmptyBoardSaysSo`, `TestTheBoardPageNeedsTheAdminToken`, `TestTheBoardPageTruncatesOutLoud`,
`TestTheParkedItemSurvivesTruncation`.

## Proof

`docs/superpowers/proofs/2026-11-22-m9-retention-and-board-proof.md`: a board with 402 events across
400 days, three items in three states, the page as a browser sees it, 311 events pruned at 90 days
with the work items untouched, and a two-hour-old witness surviving a one-hour request.

## What M9 does not do

- **No `VACUUM`.** Deleting rows frees pages for reuse; the file does not shrink. A vacuum rewrites
  the whole file under the write lock, and doing that automatically on an always-on board is a
  decision nobody has data for yet.
- **No pagination.** The page shows the newest 200 and says when there are more; a board that needs to
  browse further wants a filter, not a next-page link.
- **No retention for `threads` or `messages`.** They are bounded by their own budgets and turn limits,
  and a conversation is the kind of history an operator may want back.
- **Item actions from the page.** Approval is the only decision a page can make; cancelling, retrying
  and re-prioritising are still CLI acts.
