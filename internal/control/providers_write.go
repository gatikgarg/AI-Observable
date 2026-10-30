package control

import (
	"context"
	"fmt"
)

// CreateProvider validates and inserts a provider. The caller may leave ID,
// timestamps and display defaults empty.
func (d *DB) CreateProvider(ctx context.Context, p Provider) (Provider, error) {
	p, err := normalizeProvider(p)
	if err != nil {
		return Provider{}, err
	}
	if p.ID == "" {
		p.ID = newID()
	}
	now := nowMS()
	p.CreatedAt, p.UpdatedAt = now, now
	extra, err := encodeExtra(p.Extra)
	if err != nil {
		return Provider{}, err
	}
	_, err = d.db.ExecContext(ctx, `INSERT INTO providers(`+providerCols+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Kind, p.BaseURL, p.AuthStyle, p.AuthMode, boolInt(p.StreamUsage),
		p.ReconcileMode, p.Origin, boolInt(p.Enabled), extra, p.CreatedAt, p.UpdatedAt)
	if isUniqueViolation(err) {
		return Provider{}, fmt.Errorf("%w: provider name %q", ErrConflict, p.Name)
	}
	if err != nil {
		return Provider{}, wrap("create provider", err)
	}
	if err = d.Audit(ctx, actorOf(ctx), "provider.create", p.ID, map[string]any{
		"name": p.Name, "kind": p.Kind, "base_url": p.BaseURL, "auth_mode": p.AuthMode,
	}); err != nil {
		return Provider{}, err
	}
	if err = d.ensureJobs(ctx, p); err != nil {
		return Provider{}, err
	}
	return p, nil
}

// UpdateProvider rewrites the mutable fields. Providers imported from
// config.toml are read-only.
func (d *DB) UpdateProvider(ctx context.Context, p Provider) (Provider, error) {
	cur, err := d.GetProvider(ctx, p.ID)
	if err != nil {
		return Provider{}, err
	}
	if cur.ReadOnly() {
		return Provider{}, invalid("provider %q is imported from config.toml and is read-only", cur.Name)
	}
	p.Origin = cur.Origin
	p.CreatedAt = cur.CreatedAt
	if p.Name == "" {
		p.Name = cur.Name
	}
	if p.Kind == "" {
		p.Kind = cur.Kind
	}
	if p.BaseURL == "" {
		p.BaseURL = cur.BaseURL
	}
	if p.AuthStyle == "" {
		p.AuthStyle = cur.AuthStyle
	}
	if p.AuthMode == "" {
		p.AuthMode = cur.AuthMode
	}
	if p.ReconcileMode == "" {
		p.ReconcileMode = cur.ReconcileMode
	}
	if p.Extra == nil {
		p.Extra = cur.Extra
	}
	p, err = normalizeProvider(p)
	if err != nil {
		return Provider{}, err
	}
	p.UpdatedAt = nowMS()
	extra, err := encodeExtra(p.Extra)
	if err != nil {
		return Provider{}, err
	}
	_, err = d.db.ExecContext(ctx, `UPDATE providers SET name=?,kind=?,base_url=?,auth_style=?,
		auth_mode=?,stream_usage=?,reconcile_mode=?,enabled=?,extra_json=?,updated_at_ms=? WHERE id=?`,
		p.Name, p.Kind, p.BaseURL, p.AuthStyle, p.AuthMode, boolInt(p.StreamUsage),
		p.ReconcileMode, boolInt(p.Enabled), extra, p.UpdatedAt, p.ID)
	if isUniqueViolation(err) {
		return Provider{}, fmt.Errorf("%w: provider name %q", ErrConflict, p.Name)
	}
	if err != nil {
		return Provider{}, wrap("update provider", err)
	}
	if err = d.Audit(ctx, actorOf(ctx), "provider.update", p.ID, map[string]any{
		"name": p.Name, "kind": p.Kind, "enabled": p.Enabled, "reconcile_mode": p.ReconcileMode,
	}); err != nil {
		return Provider{}, err
	}
	return p, nil
}

// DeleteProvider removes a provider and cascades its credentials and jobs.
func (d *DB) DeleteProvider(ctx context.Context, id string) error {
	p, err := d.GetProvider(ctx, id)
	if err != nil {
		return err
	}
	if p.ReadOnly() {
		return invalid("provider %q is imported from config.toml and is read-only", p.Name)
	}
	if _, err = d.db.ExecContext(ctx, "DELETE FROM providers WHERE id=?", p.ID); err != nil {
		return wrap("delete provider", err)
	}
	return d.Audit(ctx, actorOf(ctx), "provider.delete", p.ID, map[string]any{"name": p.Name})
}

// SetProviderEnabled mutes or unmutes a provider. It is separate from
// UpdateProvider so a row imported from config.toml can be disabled from the UI
// while every other field of it stays read-only.
func (d *DB) SetProviderEnabled(ctx context.Context, id string, enabled bool) (Provider, error) {
	p, err := d.GetProvider(ctx, id)
	if err != nil {
		return Provider{}, err
	}
	if _, err = d.db.ExecContext(ctx, "UPDATE providers SET enabled=?, updated_at_ms=? WHERE id=?",
		boolInt(enabled), nowMS(), p.ID); err != nil {
		return Provider{}, wrap("set provider enabled", err)
	}
	if err = d.Audit(ctx, actorOf(ctx), "provider.enabled", p.ID, map[string]any{
		"name": p.Name, "enabled": enabled,
	}); err != nil {
		return Provider{}, err
	}
	p.Enabled = enabled
	return p, nil
}
