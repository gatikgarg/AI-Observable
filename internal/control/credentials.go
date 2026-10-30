package control

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/ai-observable/ai-observable/internal/vault"
)

// Credential is the safe view of a stored key: it never carries the secret and
// cannot be turned back into one.
type Credential struct {
	ID          string `json:"id"`
	ProviderID  string `json:"provider_id"`
	Label       string `json:"label"`
	KeyPrefix   string `json:"key_prefix"`
	KeyLast4    string `json:"key_last4"`
	Fingerprint string `json:"fingerprint"`
	Scope       string `json:"scope"`
	Status      string `json:"status"`
	LastError   string `json:"last_error,omitempty"`
	CreatedAt   int64  `json:"created_at_ms"`
	LastUsedAt  *int64 `json:"last_used_at_ms,omitempty"`
	LastOKAt    *int64 `json:"last_ok_at_ms,omitempty"`
	RevokedAt   *int64 `json:"revoked_at_ms,omitempty"`
}

// Masked is the only representation the UI and CLI print.
func (c Credential) Masked() string { return vault.Display(c.KeyPrefix, c.KeyLast4) }

// Revoked reports whether the credential is past use.
func (c Credential) Revoked() bool { return c.RevokedAt != nil }

const credentialCols = `id,provider_id,label,key_prefix,key_last4,fingerprint,scope,status,
	COALESCE(last_error,''),created_at_ms,last_used_at_ms,last_ok_at_ms,revoked_at_ms`

func scanCredential(row interface{ Scan(...any) error }) (Credential, error) {
	var c Credential
	var used, ok, revoked sql.NullInt64
	err := row.Scan(&c.ID, &c.ProviderID, &c.Label, &c.KeyPrefix, &c.KeyLast4, &c.Fingerprint,
		&c.Scope, &c.Status, &c.LastError, &c.CreatedAt, &used, &ok, &revoked)
	if err != nil {
		return Credential{}, err
	}
	if used.Valid {
		c.LastUsedAt = &used.Int64
	}
	if ok.Valid {
		c.LastOKAt = &ok.Int64
	}
	if revoked.Valid {
		c.RevokedAt = &revoked.Int64
	}
	return c, nil
}

// ListCredentials returns every credential for a provider, newest first,
// including revoked rows so the audit trail stays visible.
func (d *DB) ListCredentials(ctx context.Context, providerID string) ([]Credential, error) {
	p, err := d.GetProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	rows, err := d.db.QueryContext(ctx, "SELECT "+credentialCols+` FROM credentials
		WHERE provider_id=? ORDER BY created_at_ms DESC`, p.ID)
	if err != nil {
		return nil, wrap("list credentials", err)
	}
	defer rows.Close()
	out := []Credential{}
	for rows.Next() {
		c, serr := scanCredential(rows)
		if serr != nil {
			return nil, wrap("list credentials", serr)
		}
		out = append(out, c)
	}
	return out, wrap("list credentials", rows.Err())
}

// AddCredential seals and stores a secret. The plaintext exists only for the
// duration of this call.
func (d *DB) AddCredential(ctx context.Context, providerID, label, scope, secret string, v *vault.Vault) (Credential, error) {
	if v == nil || !v.Available() {
		return Credential{}, ErrLocked
	}
	p, err := d.GetProvider(ctx, providerID)
	if err != nil {
		return Credential{}, err
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return Credential{}, invalid("secret must not be empty")
	}
	if scope == "" {
		scope = "standard"
	}
	switch scope {
	case "standard", "admin", "provisioning":
	default:
		return Credential{}, invalid("scope %q must be standard, admin or provisioning", scope)
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = "key " + vault.Fingerprint(secret)
	}
	id := newID()
	ct, nonce, err := v.Seal(id, p.ID, []byte(secret))
	if err != nil {
		return Credential{}, err
	}
	prefix, last4 := vault.Mask(secret)
	now := nowMS()
	_, err = d.db.ExecContext(ctx, `INSERT INTO credentials(
		id,provider_id,label,secret_enc,nonce,aad,key_prefix,key_last4,fingerprint,scope,status,
		created_at_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, p.ID, label, ct, nonce, id+"\x00"+p.ID, prefix, last4, vault.Fingerprint(secret), scope,
		"unverified", now)
	if isUniqueViolation(err) {
		return Credential{}, fmt.Errorf("%w: this key is already stored for %s", ErrConflict, p.Name)
	}
	if err != nil {
		return Credential{}, wrap("add credential", err)
	}
	if _, err = d.db.ExecContext(ctx, "UPDATE providers SET updated_at_ms=? WHERE id=?", now, p.ID); err != nil {
		return Credential{}, wrap("touch provider", err)
	}
	if err = d.Audit(ctx, actorOf(ctx), "credential.create", id, map[string]any{
		"provider": p.Name, "label": label, "scope": scope, "masked": vault.Display(prefix, last4),
		"fingerprint": vault.Fingerprint(secret),
	}); err != nil {
		return Credential{}, err
	}
	return Credential{
		ID: id, ProviderID: p.ID, Label: label, KeyPrefix: prefix, KeyLast4: last4,
		Fingerprint: vault.Fingerprint(secret), Scope: scope, Status: "unverified", CreatedAt: now,
	}, nil
}
