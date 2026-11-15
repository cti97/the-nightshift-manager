-- internal/board/schema.sql
-- Migration 1. Applied verbatim when PRAGMA user_version is 0.
-- Each statement ends with a semicolon at end of line, and no statement
-- contains a semicolon internally (migrate splits on semicolons).

CREATE TABLE nodes (
  id                TEXT PRIMARY KEY,
  name              TEXT NOT NULL UNIQUE,
  token_hash        TEXT NOT NULL,
  role              TEXT NOT NULL CHECK (role IN ('manager','executor','steward')),
  provides_declared TEXT NOT NULL DEFAULT '[]',
  provides_live     TEXT NOT NULL DEFAULT '[]',
  reach_classes     TEXT NOT NULL DEFAULT '[]',
  concurrency       INTEGER NOT NULL DEFAULT 1,
  agent_version     TEXT NOT NULL DEFAULT '',
  protocol_version  INTEGER NOT NULL DEFAULT 0,
  last_seen_at      INTEGER,
  status            TEXT NOT NULL DEFAULT 'offline'
                    CHECK (status IN ('online','offline','paused','incompatible')),
  created_at        INTEGER NOT NULL
);

CREATE TABLE endpoints (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  provider      TEXT NOT NULL,
  base_url      TEXT NOT NULL,
  auth_ref      TEXT,
  reach         TEXT NOT NULL CHECK (reach IN ('host','lan','wan')),
  health        TEXT NOT NULL DEFAULT 'unknown'
                CHECK (health IN ('ok','warming','down','auth_failed','unknown')),
  health_reason TEXT,
  last_ok_at    INTEGER,
  latency_ms    INTEGER,
  created_at    INTEGER NOT NULL
);

CREATE TABLE models (
  id                TEXT PRIMARY KEY,
  endpoint_id       TEXT NOT NULL REFERENCES endpoints(id),
  model_id          TEXT NOT NULL,
  provides          TEXT NOT NULL DEFAULT '[]',
  features          TEXT NOT NULL DEFAULT '[]',
  context_window    INTEGER NOT NULL DEFAULT 0,
  cost_in_per_mtok  REAL NOT NULL DEFAULT 0,
  cost_out_per_mtok REAL NOT NULL DEFAULT 0,
  enabled           INTEGER NOT NULL DEFAULT 1,
  UNIQUE (endpoint_id, model_id)
);

CREATE TABLE work_items (
  id               TEXT PRIMARY KEY,
  parent_id        TEXT REFERENCES work_items(id),
  title            TEXT NOT NULL,
  brief            TEXT NOT NULL,
  needs            TEXT NOT NULL DEFAULT '[]',
  status           TEXT NOT NULL DEFAULT 'queued'
                   CHECK (status IN ('queued','claimed','running','needs_approval','done','failed','cancelled')),
  priority         INTEGER NOT NULL DEFAULT 0,
  deadline_at      INTEGER,
  claimed_by       TEXT REFERENCES nodes(id),
  lease_expires_at INTEGER,
  attempt          INTEGER NOT NULL DEFAULT 0,
  max_attempts     INTEGER NOT NULL DEFAULT 3,
  approval_granted TEXT,
  result_summary   TEXT,
  artifact_path    TEXT,
  blocked_reason   TEXT,
  created_by       TEXT NOT NULL,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);

CREATE INDEX idx_items_claimable ON work_items(status, priority DESC, created_at);

CREATE TABLE runs (
  id               TEXT PRIMARY KEY,
  work_item_id     TEXT NOT NULL REFERENCES work_items(id),
  attempt          INTEGER NOT NULL,
  node_id          TEXT NOT NULL,
  endpoint_id      TEXT REFERENCES endpoints(id),
  model_id         TEXT REFERENCES models(id),
  started_at       INTEGER NOT NULL,
  ended_at         INTEGER,
  status           TEXT NOT NULL
                   CHECK (status IN ('claimed','running','ok','error','lost','abandoned','escalated')),
  input_tokens     INTEGER NOT NULL DEFAULT 0,
  output_tokens    INTEGER NOT NULL DEFAULT 0,
  cost_estimate    REAL NOT NULL DEFAULT 0,
  placement_reason TEXT NOT NULL,
  transcript_path  TEXT,
  error            TEXT,
  UNIQUE (work_item_id, attempt)
);

CREATE TABLE config (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE events (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  at        INTEGER NOT NULL,
  actor     TEXT NOT NULL,
  entity    TEXT NOT NULL,
  entity_id TEXT NOT NULL,
  type      TEXT NOT NULL,
  payload   TEXT
);

CREATE INDEX idx_events_entity ON events(entity, entity_id, id);
