// Package control owns control.db: providers, encrypted credentials, collector
// jobs and the audit log. It is separate from the metrics store on purpose so a
// snapshot or a dashboard export can never ship a secret.
package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ai-observable/ai-observable/internal/vault"
)

var (
	// ErrNotFound is returned when a provider, credential or job does not exist.
	ErrNotFound = errors.New("control: not found")
	// ErrLocked mirrors vault.ErrLocked so callers can match one error.
	ErrLocked = vault.ErrLocked
	// ErrConflict is returned on a duplicate provider name or credential
	// fingerprint.
	ErrConflict = errors.New("control: already exists")
	// ErrInvalid is wrapped by validation failures.
	ErrInvalid = errors.New("control: invalid input")
)

// DB is the control database handle. All methods are safe for concurrent use.
type DB struct {
	db   *sql.DB
	path string
}

// Open opens (and migrates) <data_dir>/control.db.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("control: create %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("control: open: %w", err)
	}
	// The control DB is tiny and write-heavy from a single process. One
	// connection removes lock contention on Windows and keeps every PRAGMA
	// (foreign_keys, busy_timeout) in force for every statement.
	db.SetMaxOpenConns(1)
	d := &DB{db: db, path: path}
	if err = d.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

// Close flushes WAL and closes the handle.
func (d *DB) Close() error {
	if d == nil || d.db == nil {
		return nil
	}
	if _, err := d.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		d.db.Close()
		return err
	}
	return d.db.Close()
}

// Path is the file this DB was opened from.
func (d *DB) Path() string { return d.path }

// Ping reports whether the database still answers.
func (d *DB) Ping(ctx context.Context) error { return d.db.PingContext(ctx) }

// Meta reads one meta row. A missing key returns "" and no error: every meta
// value has a defined default.
func (d *DB) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := d.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key=?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("control: read meta %s: %w", key, err)
	}
	return v, nil
}

// SetMeta writes one meta row.
func (d *DB) SetMeta(ctx context.Context, key, value string) error {
	_, err := d.db.ExecContext(ctx,
		"INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	if err != nil {
		return fmt.Errorf("control: write meta %s: %w", key, err)
	}
	return nil
}

// CheckVault compares the vault key with the recorded master KCV and records it
// the first time. A mismatch returns vault.ErrKeyMismatch: the key file is never
// replaced automatically.
func (d *DB) CheckVault(ctx context.Context, v *vault.Vault) error {
	if v == nil || !v.Available() {
		return nil
	}
	stored, err := d.Meta(ctx, "master_kcv")
	if err != nil {
		return err
	}
	if stored == "" {
		if err = d.SetMeta(ctx, "master_kcv", v.KCV()); err != nil {
			return err
		}
		return d.SetMeta(ctx, "master_mode", modeOf(v))
	}
	if err = v.VerifyKCV(stored); err != nil {
		return err
	}
	return d.SetMeta(ctx, "master_mode", modeOf(v))
}

func modeOf(v *vault.Vault) string {
	if v.Path() == "" {
		return "env"
	}
	return "file"
}

// actorKey is the context key for the audit actor.
type actorKey struct{}

// WithActor labels every audit row written by calls made with the returned
// context. Callers use "ui", "cli" or "collector"; the default is "system".
func WithActor(ctx context.Context, actor string) context.Context {
	if actor == "" {
		actor = "system"
	}
	return context.WithValue(ctx, actorKey{}, actor)
}

func actorOf(ctx context.Context) string {
	if a, ok := ctx.Value(actorKey{}).(string); ok && a != "" {
		return a
	}
	return "system"
}

// nowMS is the single clock used by every write, so tests can reason about
// ordering without sleeping.
func nowMS() int64 { return time.Now().UnixMilli() }

// boolInt renders a bool for SQLite's INTEGER columns.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// wrap adds context to a driver error without losing the sentinel.
func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("control: %s: %w", op, err)
}

// invalid wraps ErrInvalid with a human sentence.
func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// isUniqueViolation detects the driver's duplicate-key message. modernc.org/sqlite
// reports "constraint failed: UNIQUE constraint failed: <table>.<col>".
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "unique constraint failed") || strings.Contains(s, "constraint failed: unique")
}
