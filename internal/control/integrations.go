package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Integration is the token-free registry view. TokenHash is never serialized
// by the management API.
type Integration struct {
	ID               string `json:"id"`
	AgentType        string `json:"agent_type"`
	Label            string `json:"label"`
	ProviderConfigID string `json:"provider_config_id"`
	Model            string `json:"model"`
	TokenHash        string `json:"-"`
	TokenPrefix      string `json:"token_prefix"`
	CaptureMode      string `json:"capture_mode"`
	State            string `json:"state"`
	CreatedAtMS      int64  `json:"created_at_ms"`
	FirstSeenAtMS    *int64 `json:"first_seen_at_ms,omitempty"`
	LastSeenAtMS     *int64 `json:"last_seen_at_ms,omitempty"`
	AppliedBackupID  string `json:"applied_backup_id,omitempty"`
	PortPinned       bool   `json:"port_pinned"`
	GatewayBaseURL   string `json:"gateway_base_url"`
	RevokedAtMS      *int64 `json:"revoked_at_ms,omitempty"`
}

func (i Integration) Revoked() bool { return i.RevokedAtMS != nil }

type IntegrationBackup struct {
	ID            string
	IntegrationID string
	CreatedAtMS   int64
	ManifestJSON  string
}

const integrationCols = `id,agent_type,label,provider_config_id,model,token_hash,token_prefix,
	capture_mode,state,created_at_ms,first_seen_at_ms,last_seen_at_ms,applied_backup_id,
	port_pinned,gateway_base_url,revoked_at_ms`

func scanIntegration(row interface{ Scan(...any) error }) (Integration, error) {
	var i Integration
	var first, last, revoked sql.NullInt64
	var pinned int
	var backup sql.NullString
	err := row.Scan(&i.ID, &i.AgentType, &i.Label, &i.ProviderConfigID, &i.Model,
		&i.TokenHash, &i.TokenPrefix, &i.CaptureMode, &i.State, &i.CreatedAtMS,
		&first, &last, &backup, &pinned, &i.GatewayBaseURL, &revoked)
	if err != nil {
		return Integration{}, err
	}
	i.PortPinned = pinned != 0
	if first.Valid {
		i.FirstSeenAtMS = &first.Int64
	}
	if last.Valid {
		i.LastSeenAtMS = &last.Int64
	}
	if backup.Valid {
		i.AppliedBackupID = backup.String
	}
	if revoked.Valid {
		i.RevokedAtMS = &revoked.Int64
	}
	return i, nil
}

func (d *DB) CreateIntegration(ctx context.Context, i Integration) (Integration, error) {
	if i.ID == "" {
		i.ID = newID()
	}
	if i.CreatedAtMS == 0 {
		i.CreatedAtMS = nowMS()
	}
	if err := validateIntegration(i); err != nil {
		return Integration{}, err
	}
	if _, err := d.GetProvider(ctx, i.ProviderConfigID); err != nil {
		return Integration{}, err
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO integrations(
		id,agent_type,label,provider_config_id,model,token_hash,token_prefix,capture_mode,state,
		created_at_ms,port_pinned,gateway_base_url) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		i.ID, i.AgentType, i.Label, i.ProviderConfigID, i.Model, i.TokenHash, i.TokenPrefix,
		i.CaptureMode, i.State, i.CreatedAtMS, boolInt(i.PortPinned), i.GatewayBaseURL)
	if isUniqueViolation(err) {
		return Integration{}, fmt.Errorf("%w: token or integration already exists", ErrConflict)
	}
	if err != nil {
		return Integration{}, wrap("create integration", err)
	}
	if err = d.Audit(ctx, actorOf(ctx), "integration.create", i.ID, map[string]any{
		"agent_type": i.AgentType, "label": i.Label, "provider": i.ProviderConfigID,
		"capture_mode": i.CaptureMode,
	}); err != nil {
		return Integration{}, err
	}
	return i, nil
}

func validateIntegration(i Integration) error {
	switch i.AgentType {
	case "codex", "cline", "kilo-code":
	default:
		return invalid("agent_type must be codex, cline or kilo-code")
	}
	switch i.CaptureMode {
	case "none", "truncated":
	default:
		return invalid("capture_mode must be none or truncated")
	}
	switch i.State {
	case "planned", "applied", "needs_attention", "disconnected", "verified":
	default:
		return invalid("invalid integration state %q", i.State)
	}
	if strings.TrimSpace(i.Label) == "" || len(i.Label) > 48 {
		return invalid("label must be 1-48 characters")
	}
	if strings.TrimSpace(i.TokenHash) == "" || strings.TrimSpace(i.TokenPrefix) == "" {
		return invalid("token hash and prefix are required")
	}
	if strings.TrimSpace(i.GatewayBaseURL) == "" {
		return invalid("gateway_base_url is required")
	}
	return nil
}

func (d *DB) GetIntegration(ctx context.Context, id string) (Integration, error) {
	i, err := scanIntegration(d.db.QueryRowContext(ctx,
		"SELECT "+integrationCols+" FROM integrations WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Integration{}, fmt.Errorf("%w: integration %q", ErrNotFound, id)
	}
	if err != nil {
		return Integration{}, wrap("get integration", err)
	}
	return i, nil
}

func (d *DB) ListIntegrations(ctx context.Context) ([]Integration, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT "+integrationCols+" FROM integrations ORDER BY created_at_ms DESC")
	if err != nil {
		return nil, wrap("list integrations", err)
	}
	defer rows.Close()
	out := []Integration{}
	for rows.Next() {
		i, err := scanIntegration(rows)
		if err != nil {
			return nil, wrap("list integrations", err)
		}
		out = append(out, i)
	}
	return out, wrap("list integrations", rows.Err())
}

// IntegrationByTokenHash resolves only active tokens. The caller still uses a
// constant-time comparison against the returned hash.
func (d *DB) IntegrationByTokenHash(ctx context.Context, hash string) (Integration, error) {
	i, err := scanIntegration(d.db.QueryRowContext(ctx,
		"SELECT "+integrationCols+" FROM integrations WHERE token_hash=? AND revoked_at_ms IS NULL", hash))
	if errors.Is(err, sql.ErrNoRows) {
		return Integration{}, fmt.Errorf("%w: gateway token", ErrNotFound)
	}
	if err != nil {
		return Integration{}, wrap("resolve gateway token", err)
	}
	return i, nil
}

func (d *DB) SetIntegrationToken(ctx context.Context, id, hash, prefix string) error {
	res, err := d.db.ExecContext(ctx, `UPDATE integrations SET token_hash=?,token_prefix=?,
		state='planned',revoked_at_ms=NULL WHERE id=?`, hash, prefix, id)
	if err != nil {
		return wrap("rotate integration token", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: integration %q", ErrNotFound, id)
	}
	return d.Audit(ctx, actorOf(ctx), "integration.token.rotate", id, nil)
}

func (d *DB) SetIntegrationState(ctx context.Context, id, state, backupID string) error {
	switch state {
	case "planned", "applied", "needs_attention", "disconnected", "verified":
	default:
		return invalid("invalid integration state %q", state)
	}
	res, err := d.db.ExecContext(ctx, `UPDATE integrations SET state=?,applied_backup_id=COALESCE(NULLIF(?,''),applied_backup_id)
		WHERE id=?`, state, backupID, id)
	if err != nil {
		return wrap("set integration state", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: integration %q", ErrNotFound, id)
	}
	return nil
}

func (d *DB) MarkIntegrationTraffic(ctx context.Context, id string) error {
	now := nowMS()
	_, err := d.db.ExecContext(ctx, `UPDATE integrations SET
		first_seen_at_ms=COALESCE(first_seen_at_ms,?),last_seen_at_ms=?,state='verified'
		WHERE id=? AND revoked_at_ms IS NULL AND state<>'disconnected'`, now, now, id)
	return wrap("mark integration traffic", err)
}

func (d *DB) RevokeIntegration(ctx context.Context, id string) error {
	res, err := d.db.ExecContext(ctx, `UPDATE integrations SET revoked_at_ms=?,state='disconnected'
		WHERE id=? AND revoked_at_ms IS NULL`, nowMS(), id)
	if err != nil {
		return wrap("revoke integration", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err = d.GetIntegration(ctx, id); err != nil {
			return err
		}
		return nil
	}
	return d.Audit(ctx, actorOf(ctx), "integration.revoke", id, nil)
}

func (d *DB) AddIntegrationBackup(ctx context.Context, b IntegrationBackup) (IntegrationBackup, error) {
	if b.ID == "" {
		b.ID = newID()
	}
	if b.CreatedAtMS == 0 {
		b.CreatedAtMS = nowMS()
	}
	_, err := d.db.ExecContext(ctx, `INSERT INTO integration_backups(
		id,integration_id,created_at_ms,manifest_json) VALUES(?,?,?,?)`,
		b.ID, b.IntegrationID, b.CreatedAtMS, b.ManifestJSON)
	if err != nil {
		return IntegrationBackup{}, wrap("add integration backup", err)
	}
	return b, nil
}

func (d *DB) GetIntegrationBackup(ctx context.Context, id string) (IntegrationBackup, error) {
	var b IntegrationBackup
	err := d.db.QueryRowContext(ctx, `SELECT id,integration_id,created_at_ms,manifest_json
		FROM integration_backups WHERE id=?`, id).
		Scan(&b.ID, &b.IntegrationID, &b.CreatedAtMS, &b.ManifestJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return IntegrationBackup{}, fmt.Errorf("%w: backup %q", ErrNotFound, id)
	}
	if err != nil {
		return IntegrationBackup{}, wrap("get integration backup", err)
	}
	return b, nil
}
