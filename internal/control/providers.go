package control

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Provider is one configured upstream endpoint.
type Provider struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`
	BaseURL       string            `json:"base_url"`
	AuthStyle     string            `json:"auth_style"`
	AuthMode      string            `json:"auth_mode"`
	ReconcileMode string            `json:"reconcile_mode"`
	Origin        string            `json:"origin"`
	StreamUsage   bool              `json:"stream_usage"`
	Enabled       bool              `json:"enabled"`
	Extra         map[string]string `json:"extra,omitempty"`
	CreatedAt     int64             `json:"created_at_ms"`
	UpdatedAt     int64             `json:"updated_at_ms"`
}

// ReadOnly reports whether the row was imported from config.toml and therefore
// cannot be edited or deleted from the UI or CLI.
func (p Provider) ReadOnly() bool { return p.Origin == "config" }

const providerCols = `id,name,kind,base_url,auth_style,auth_mode,stream_usage,reconcile_mode,
	origin,enabled,extra_json,created_at_ms,updated_at_ms`

func scanProvider(row interface{ Scan(...any) error }) (Provider, error) {
	var p Provider
	var stream, enabled int
	var extra sql.NullString
	err := row.Scan(&p.ID, &p.Name, &p.Kind, &p.BaseURL, &p.AuthStyle, &p.AuthMode, &stream,
		&p.ReconcileMode, &p.Origin, &enabled, &extra, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return Provider{}, err
	}
	p.StreamUsage = stream == 1
	p.Enabled = enabled == 1
	if extra.Valid && extra.String != "" {
		if err = json.Unmarshal([]byte(extra.String), &p.Extra); err != nil {
			return Provider{}, fmt.Errorf("control: provider %s extra_json: %w", p.ID, err)
		}
	}
	return p, nil
}

// ListProviders returns every provider, ordered by name.
func (d *DB) ListProviders(ctx context.Context) ([]Provider, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+providerCols+" FROM providers ORDER BY name")
	if err != nil {
		return nil, wrap("list providers", err)
	}
	defer rows.Close()
	out := []Provider{}
	for rows.Next() {
		p, serr := scanProvider(rows)
		if serr != nil {
			return nil, wrap("list providers", serr)
		}
		out = append(out, p)
	}
	return out, wrap("list providers", rows.Err())
}

// GetProvider resolves a provider by id or by name.
func (d *DB) GetProvider(ctx context.Context, idOrName string) (Provider, error) {
	idOrName = strings.TrimSpace(idOrName)
	if idOrName == "" {
		return Provider{}, invalid("provider id or name is required")
	}
	p, err := scanProvider(d.db.QueryRowContext(ctx,
		"SELECT "+providerCols+" FROM providers WHERE id=?", idOrName))
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Provider{}, wrap("get provider", err)
	}
	p, err = scanProvider(d.db.QueryRowContext(ctx,
		"SELECT "+providerCols+" FROM providers WHERE name=?", idOrName))
	if errors.Is(err, sql.ErrNoRows) {
		return Provider{}, fmt.Errorf("%w: provider %q", ErrNotFound, idOrName)
	}
	if err != nil {
		return Provider{}, wrap("get provider", err)
	}
	return p, nil
}
