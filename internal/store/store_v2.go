package store

import (
	"context"
	"database/sql"
	"errors"
)

// Window is a half-open aggregation window. Granularity is "hour" or "day".
type Window struct {
	StartMS, EndMS int64
	Granularity    string
}

// UsageRow is one provider-reported bucket (lane B). A nil field means the
// provider did not report that value and the column stays NULL; it is never 0.
type UsageRow struct {
	Provider, Model, Project, APIKeyID string
	BucketStartMS, BucketEndMS         int64
	Granularity                        string
	Requests, Input, Output            *int64
	CacheRead, CacheWrite              *int64
	CostUSD                            *float64
	Currency, Source, RawHash          string
}

// CatalogRow is one model catalog entry (lane C).
type CatalogRow struct {
	Provider, ModelID, DisplayName                           string
	ContextMax                                               *int64
	InputPrice, OutputPrice, CacheReadPrice, CacheWritePrice *float64
	PriceSource, Capabilities                                string
	FirstSeenMS, LastSeenMS                                  int64
}

// ImportRow is one entry of the import ledger.
type ImportRow struct {
	Provider, Kind             string
	WindowStartMS, WindowEndMS int64
	Status                     string
	RowsImported               int64
	RequestsImported           int64
	CostImportedUSD            *float64
	ErrText                    string
	StartedAtMS                int64
	FinishedAtMS               *int64
}

// migrateV2 adds the lane B/C tables and the store metadata table. It is
// additive and idempotent; calls and calls_v are never touched here.
func migrateV2(db *sql.DB) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS provider_usage(
  id                 INTEGER PRIMARY KEY,
  provider           TEXT    NOT NULL,
  model              TEXT,
  project            TEXT,
  api_key_id         TEXT,
  bucket_start_ms    INTEGER NOT NULL,
  bucket_end_ms      INTEGER NOT NULL,
  granularity        TEXT    NOT NULL,
  requests           INTEGER,
  input_tokens       INTEGER,
  output_tokens      INTEGER,
  cache_read_tokens  INTEGER,
  cache_write_tokens INTEGER,
  cost_usd           REAL,
  currency           TEXT,
  source             TEXT    NOT NULL,
  raw_hash           TEXT,
  imported_at_ms     INTEGER NOT NULL,
  UNIQUE(provider, model, project, bucket_start_ms, bucket_end_ms, source)
)`,
		`CREATE INDEX IF NOT EXISTS idx_pu_provider_time ON provider_usage(provider, bucket_start_ms)`,
		`CREATE TABLE IF NOT EXISTS provider_usage_imports(
  id                 INTEGER PRIMARY KEY,
  provider           TEXT    NOT NULL,
  kind               TEXT    NOT NULL,
  window_start_ms    INTEGER NOT NULL,
  window_end_ms      INTEGER NOT NULL,
  status             TEXT    NOT NULL,
  rows_imported      INTEGER NOT NULL DEFAULT 0,
  requests_imported  INTEGER NOT NULL DEFAULT 0,
  cost_imported_usd  REAL,
  error              TEXT,
  started_at_ms      INTEGER NOT NULL,
  finished_at_ms     INTEGER,
  UNIQUE(provider, kind, window_start_ms, window_end_ms)
)`,
		`CREATE TABLE IF NOT EXISTS model_catalog(
  provider          TEXT    NOT NULL,
  model_id          TEXT    NOT NULL,
  display_name      TEXT,
  context_max       INTEGER,
  input_price       REAL,
  output_price      REAL,
  cache_read_price  REAL,
  cache_write_price REAL,
  price_source      TEXT,
  capabilities      TEXT,
  first_seen_ms     INTEGER,
  last_seen_ms      INTEGER,
  PRIMARY KEY(provider, model_id)
)`,
	} {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

func filepathToSlash(p string) string {
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		if p[i] == '\\' {
			out = append(out, '/')
			continue
		}
		out = append(out, p[i])
	}
	return string(out)
}

// OpenReadOnly opens an existing database with ?mode=ro, which is what the
// admin read paths and doctor use.
func OpenReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+filepathToSlash(path)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// SetMeta stores a durable store-level value such as the retention cursor.
func (s *Store) SetMeta(ctx context.Context, k, v string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v)
	return err
}

// Meta reads a value written by SetMeta. A missing key is not an error.
func (s *Store) Meta(ctx context.Context, k string) (string, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}
