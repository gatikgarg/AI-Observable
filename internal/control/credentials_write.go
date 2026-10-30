package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SetCredentialStatus records the result of a validation probe. status='ok'
// stamps last_ok_at_ms; any other status records the provider's message.
func (d *DB) SetCredentialStatus(ctx context.Context, credID, status, lastError string) error {
	switch status {
	case "unverified", "ok", "invalid", "insufficient_scope", "rate_limited", "error":
	default:
		return invalid("credential status %q is not one of unverified, ok, invalid, insufficient_scope, rate_limited, error", status)
	}
	now := nowMS()
	var res sql.Result
	var err error
	if status == "ok" {
		res, err = d.db.ExecContext(ctx, `UPDATE credentials SET status=?,last_error=NULL,
			last_ok_at_ms=?,last_used_at_ms=COALESCE(last_used_at_ms,?) WHERE id=?`,
			status, now, now, credID)
	} else {
		res, err = d.db.ExecContext(ctx,
			"UPDATE credentials SET status=?,last_error=? WHERE id=?", status, nullIfEmpty(lastError), credID)
	}
	if err != nil {
		return wrap("set credential status", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: credential %q", ErrNotFound, credID)
	}
	return d.Audit(ctx, actorOf(ctx), "credential.status", credID, map[string]any{
		"status": status, "error": lastError,
	})
}

// TouchCredential stamps last_used_at_ms after a successful injection.
func (d *DB) TouchCredential(ctx context.Context, credID string) error {
	_, err := d.db.ExecContext(ctx, "UPDATE credentials SET last_used_at_ms=? WHERE id=?", nowMS(), credID)
	if err != nil {
		return wrap("touch credential", err)
	}
	return nil
}

// RevokeCredential marks a key unusable but keeps the row for the audit trail.
func (d *DB) RevokeCredential(ctx context.Context, credID string) error {
	now := nowMS()
	res, err := d.db.ExecContext(ctx, `UPDATE credentials SET revoked_at_ms=?,last_error=NULL
		WHERE id=? AND revoked_at_ms IS NULL`, now, credID)
	if err != nil {
		return wrap("revoke credential", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists int
		if qerr := d.db.QueryRowContext(ctx, "SELECT 1 FROM credentials WHERE id=?", credID).Scan(&exists); errors.Is(qerr, sql.ErrNoRows) {
			return fmt.Errorf("%w: credential %q", ErrNotFound, credID)
		}
		return nil // already revoked: idempotent
	}
	if _, err = d.db.ExecContext(ctx, `UPDATE providers SET updated_at_ms=?
		WHERE id=(SELECT provider_id FROM credentials WHERE id=?)`, now, credID); err != nil {
		return wrap("touch provider", err)
	}
	return d.Audit(ctx, actorOf(ctx), "credential.revoke", credID, nil)
}

// GetCredential returns one credential row without its secret.
func (d *DB) GetCredential(ctx context.Context, credID string) (Credential, error) {
	c, err := scanCredential(d.db.QueryRowContext(ctx,
		"SELECT "+credentialCols+" FROM credentials WHERE id=?", credID))
	if errors.Is(err, sql.ErrNoRows) {
		return Credential{}, fmt.Errorf("%w: credential %q", ErrNotFound, credID)
	}
	if err != nil {
		return Credential{}, wrap("get credential", err)
	}
	return c, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
