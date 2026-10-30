package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// schemaVersion is bumped whenever a statement below changes shape. Migrations
// are additive: existing rows are never rewritten.
const schemaVersion = 2

// dsn builds a file: URI carrying per-connection pragmas, so every pooled
// connection opens with foreign keys on and a busy timeout instead of relying on
// a one-off Exec that only affects one connection.
func dsn(path string) string {
	abs := filepath.ToSlash(path)
	if !strings.HasPrefix(abs, "/") {
		abs = "/" + abs
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// pragmas are applied by the driver to every new connection. busy_timeout is
// sorted first by the driver; the order here is documentation only.
var pragmas = []string{
	"busy_timeout(5000)",
	"foreign_keys(1)",
	"journal_mode(WAL)",
	"synchronous(FULL)",
}

// schema is the complete control schema. Every statement is idempotent so a
// half-created database repairs itself on the next start.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS meta(
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS providers(
		id             TEXT PRIMARY KEY,
		name           TEXT NOT NULL UNIQUE,
		kind           TEXT NOT NULL,
		base_url       TEXT NOT NULL,
		auth_style     TEXT NOT NULL DEFAULT 'bearer',
		auth_mode      TEXT NOT NULL DEFAULT 'managed',
		stream_usage   INTEGER NOT NULL DEFAULT 1,
		reconcile_mode TEXT NOT NULL DEFAULT 'off',
		origin         TEXT NOT NULL DEFAULT 'ui',
		enabled        INTEGER NOT NULL DEFAULT 1,
		extra_json     TEXT,
		created_at_ms  INTEGER NOT NULL,
		updated_at_ms  INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS credentials(
		id              TEXT PRIMARY KEY,
		provider_id     TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
		label           TEXT NOT NULL,
		secret_enc      BLOB NOT NULL,
		nonce           BLOB NOT NULL,
		aad             TEXT NOT NULL,
		key_prefix      TEXT NOT NULL,
		key_last4       TEXT NOT NULL,
		fingerprint     TEXT NOT NULL,
		scope           TEXT NOT NULL DEFAULT 'standard',
		status          TEXT NOT NULL DEFAULT 'unverified',
		last_error      TEXT,
		created_at_ms   INTEGER NOT NULL,
		last_used_at_ms INTEGER,
		last_ok_at_ms   INTEGER,
		revoked_at_ms   INTEGER
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_cred_fp ON credentials(provider_id, fingerprint)`,
	`CREATE INDEX IF NOT EXISTS idx_cred_provider ON credentials(provider_id, created_at_ms)`,
	`CREATE TABLE IF NOT EXISTS collector_jobs(
		id                   TEXT PRIMARY KEY,
		provider_id          TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
		kind                 TEXT NOT NULL,
		enabled              INTEGER NOT NULL DEFAULT 1,
		interval_seconds     INTEGER NOT NULL,
		cursor_ms            INTEGER,
		next_run_at_ms       INTEGER,
		consecutive_failures INTEGER NOT NULL DEFAULT 0,
		last_run_at_ms       INTEGER,
		last_status          TEXT,
		last_error           TEXT,
		UNIQUE(provider_id, kind)
	)`,
	`CREATE TABLE IF NOT EXISTS audit(
		id     INTEGER PRIMARY KEY,
		ts_ms  INTEGER NOT NULL,
		actor  TEXT,
		action TEXT NOT NULL,
		target TEXT,
		detail TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit(ts_ms)`,
	`CREATE TABLE IF NOT EXISTS integrations(
		id                 TEXT PRIMARY KEY,
		agent_type         TEXT NOT NULL,
		label              TEXT NOT NULL,
		provider_config_id TEXT NOT NULL REFERENCES providers(id),
		model              TEXT NOT NULL DEFAULT '',
		token_hash         TEXT NOT NULL UNIQUE,
		token_prefix       TEXT NOT NULL,
		capture_mode       TEXT NOT NULL DEFAULT 'none',
		state              TEXT NOT NULL,
		created_at_ms      INTEGER NOT NULL,
		first_seen_at_ms   INTEGER,
		last_seen_at_ms    INTEGER,
		applied_backup_id  TEXT,
		port_pinned        INTEGER NOT NULL DEFAULT 1,
		gateway_base_url   TEXT NOT NULL,
		revoked_at_ms      INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_integrations_last_seen
		ON integrations(last_seen_at_ms)`,
	`CREATE TABLE IF NOT EXISTS integration_backups(
		id             TEXT PRIMARY KEY,
		integration_id TEXT NOT NULL REFERENCES integrations(id) ON DELETE CASCADE,
		created_at_ms  INTEGER NOT NULL,
		manifest_json  TEXT NOT NULL
	)`,
}

func (d *DB) migrate(ctx context.Context) error {
	for _, stmt := range schema {
		if _, err := d.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("control: migrate: %w", err)
		}
	}
	stored, err := d.Meta(ctx, "schema_version")
	if err != nil {
		return err
	}
	if stored == "" {
		if err = d.SetMeta(ctx, "created_at_ms", fmt.Sprint(nowMS())); err != nil {
			return err
		}
	}
	if stored != fmt.Sprint(schemaVersion) {
		if err = d.SetMeta(ctx, "schema_version", fmt.Sprint(schemaVersion)); err != nil {
			return err
		}
	}
	if err = d.SetMeta(ctx, "data_mode", "control"); err != nil {
		return err
	}
	return d.seedJobs(ctx)
}

// newID returns a UUID v4 built from crypto/rand: no dependency, no collision
// risk that matters at this scale.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is fatal for a product whose whole job is keeping
		// secrets apart; a predictable ID is worse than a crash.
		panic("control: no system entropy: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
