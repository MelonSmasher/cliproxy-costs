package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

// migrations are forward-only; index i migrates schema version i → i+1.
var migrations = []func(ctx context.Context, tx *sql.Tx) error{
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS requests (
  request_id      TEXT PRIMARY KEY,
  trace_id        TEXT NOT NULL,
  requested_at_ms INTEGER NOT NULL,
  provider        TEXT NOT NULL,
  executor_type   TEXT,
  model           TEXT NOT NULL,
  alias           TEXT,
  response_model  TEXT,
  auth_id         TEXT,
  credential      TEXT,
  auth_type       TEXT,
  client          TEXT,
  session_id      TEXT,
  stream          INTEGER NOT NULL,
  generate        INTEGER NOT NULL,
  failed          INTEGER NOT NULL,
  failure_status  INTEGER,
  latency_ms      REAL,
  ttft_ms         REAL,
  t_input         INTEGER NOT NULL,
  t_cache_read    INTEGER NOT NULL,
  t_cache_write   INTEGER NOT NULL,
  t_output        INTEGER NOT NULL,
  t_reasoning     INTEGER NOT NULL,
  token_mismatch  INTEGER NOT NULL DEFAULT 0,
  c_input REAL, c_cache_read REAL, c_cache_write REAL, c_output REAL, c_total REAL,
  pricing_status  TEXT NOT NULL,
  rate_card_id    TEXT,
  tier_above      INTEGER,
  catalog_ref     TEXT
) STRICT;
CREATE INDEX IF NOT EXISTS requests_trace ON requests(trace_id);
CREATE INDEX IF NOT EXISTS requests_time ON requests(requested_at_ms);
CREATE INDEX IF NOT EXISTS requests_model_time ON requests(model, requested_at_ms);

CREATE TABLE IF NOT EXISTS rate_cards (
  id TEXT PRIMARY KEY, catalog_ref TEXT, card_json TEXT NOT NULL, feed_etag TEXT, first_seen_ms INTEGER NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS daily_rollups (
  day TEXT NOT NULL, model TEXT NOT NULL, provider TEXT NOT NULL,
  credential TEXT NOT NULL DEFAULT '', client TEXT NOT NULL DEFAULT '',
  requests INTEGER NOT NULL, failed INTEGER NOT NULL, unpriced INTEGER NOT NULL,
  t_input INTEGER NOT NULL, t_cache_read INTEGER NOT NULL, t_cache_write INTEGER NOT NULL,
  t_output INTEGER NOT NULL, t_reasoning INTEGER NOT NULL,
  c_total REAL NOT NULL,
  PRIMARY KEY (day, model, provider, credential, client)
) STRICT, WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS quota_snapshots (
  credential TEXT PRIMARY KEY, auth_id TEXT, provider TEXT NOT NULL,
  observed_at_ms INTEGER NOT NULL, snapshot_json TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS feed_snapshot (
  id INTEGER PRIMARY KEY CHECK (id = 1), etag TEXT, fetched_at_ms INTEGER NOT NULL, body_zstd BLOB NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS feed_status (
  id INTEGER PRIMARY KEY CHECK (id = 1), last_attempt_ms INTEGER, last_error TEXT
) STRICT;

CREATE TABLE IF NOT EXISTS learned_models (
  model TEXT PRIMARY KEY, catalog_provider TEXT NOT NULL, updated_ms INTEGER NOT NULL
) STRICT;
`)
		return err
	},
	// 2: remember which feed URL a snapshot came from, so a changed feed-url
	// is fetched immediately instead of waiting for the refresh interval.
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ALTER TABLE feed_snapshot ADD COLUMN url TEXT`)
		return err
	},
	// 3: last good ECB reference rates (display-only currency conversion).
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
CREATE TABLE fx_snapshot (
  id INTEGER PRIMARY KEY CHECK (id = 1), url TEXT NOT NULL, last_modified TEXT, fetched_at_ms INTEGER NOT NULL,
  as_of TEXT NOT NULL, per_eur_json TEXT NOT NULL
) STRICT;
CREATE TABLE fx_status (
  id INTEGER PRIMARY KEY CHECK (id = 1), last_attempt_ms INTEGER, last_error TEXT
) STRICT;
`)
		return err
	},
	// 4: failed-only recent lookups (dashboard "failures only" filter and summary.failures).
	func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS requests_failed_time ON requests(requested_at_ms) WHERE failed = 1`)
		return err
	},
}

// SchemaVersion is the current store schema version.
var SchemaVersion = len(migrations)

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT`); err != nil {
		return err
	}
	current := 0
	var v string
	switch err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&v); err {
	case nil:
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("store: bad schema_version %q", v)
		}
		current = n
	case sql.ErrNoRows:
	default:
		return err
	}
	if current > len(migrations) {
		return fmt.Errorf("store: database schema %d is newer than this plugin (%d)", current, len(migrations))
	}
	for i := current; i < len(migrations); i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if err := migrations[i](ctx, tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('schema_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.Itoa(i+1)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO meta(key,value) VALUES('created_at',?)`, strconv.FormatInt(nowMS(), 10)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
