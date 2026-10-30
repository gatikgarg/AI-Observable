package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ai-observable/ai-observable/internal/vault"
)

// Secret decrypts one credential by id. The AAD column is verified before the
// vault is asked to open the blob, so a row moved between credentials fails at
// the first check.
func (d *DB) Secret(ctx context.Context, credID string, v *vault.Vault) (string, error) {
	if v == nil || !v.Available() {
		return "", ErrLocked
	}
	var providerID, aad string
	var ct, nonce []byte
	err := d.db.QueryRowContext(ctx,
		"SELECT provider_id,aad,secret_enc,nonce FROM credentials WHERE id=?", credID).
		Scan(&providerID, &aad, &ct, &nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: credential %q", ErrNotFound, credID)
	}
	if err != nil {
		return "", wrap("read credential", err)
	}
	if want := credID + "\x00" + providerID; aad != want {
		return "", fmt.Errorf("control: credential %s has an unexpected aad", credID)
	}
	plain, err := v.Open(credID, providerID, ct, nonce)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// ActiveSecretFor returns the key the proxy injects for a provider: the newest
// non-revoked credential that is not known-bad, preferring status='ok'.
//
// It returns ErrLocked when the vault is unavailable so the caller can degrade
// to pass-through rather than crash.
func (d *DB) ActiveSecretFor(ctx context.Context, providerID string, v *vault.Vault) (secret, credID string, err error) {
	if v == nil || !v.Available() {
		return "", "", ErrLocked
	}
	p, err := d.GetProvider(ctx, providerID)
	if err != nil {
		return "", "", err
	}
	var aad string
	var ct, nonce []byte
	err = d.db.QueryRowContext(ctx, `SELECT id,aad,secret_enc,nonce FROM credentials
		WHERE provider_id=? AND revoked_at_ms IS NULL AND status<>'invalid'
		ORDER BY CASE status WHEN 'ok' THEN 0 WHEN 'unverified' THEN 1 ELSE 2 END, created_at_ms DESC
		LIMIT 1`, p.ID).Scan(&credID, &aad, &ct, &nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("%w: no usable credential for %s", ErrNotFound, p.Name)
	}
	if err != nil {
		return "", "", wrap("select credential", err)
	}
	if want := credID + "\x00" + p.ID; aad != want {
		return "", "", fmt.Errorf("control: credential %s has an unexpected aad", credID)
	}
	plain, err := v.Open(credID, p.ID, ct, nonce)
	if err != nil {
		return "", "", err
	}
	return string(plain), credID, nil
}
