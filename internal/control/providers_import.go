package control

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/ai-observable/ai-observable/internal/config"
	"github.com/ai-observable/ai-observable/internal/provider"
)

// ImportFromConfig adds providers declared in config.toml as origin=config,
// auth_mode=client rows and refreshes their base URL on later starts. It never
// overwrites a provider created in the UI and returns how many rows it touched.
//
// config.toml carries no kind, so a provider whose name is a known kind takes
// that kind and everything else becomes openai-compatible. Imports pass the
// client's own key through, which is how these providers behaved before the
// API Manager existed.
func (d *DB) ImportFromConfig(ctx context.Context, providers map[string]config.Provider) (int, error) {
	touched := 0
	now := nowMS()
	for name, cp := range providers {
		if !config.ProviderName.MatchString(name) {
			continue
		}
		kind := name
		preset, ok := provider.PresetFor(kind)
		if !ok {
			kind = "openai-compatible"
			preset, _ = provider.PresetFor(kind)
		}
		base := strings.TrimRight(strings.TrimSpace(cp.Upstream), "/")
		if base == "" {
			base = preset.DefaultBaseURL
		}
		if validBaseURL(base) != nil {
			continue
		}
		res, err := d.db.ExecContext(ctx, `INSERT INTO providers(
			id,name,kind,base_url,auth_style,auth_mode,stream_usage,reconcile_mode,origin,enabled,
			extra_json,created_at_ms,updated_at_ms)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(name) DO UPDATE SET
				kind=excluded.kind, base_url=excluded.base_url, stream_usage=excluded.stream_usage,
				updated_at_ms=excluded.updated_at_ms
			WHERE providers.origin='config'
			  AND (providers.kind<>excluded.kind OR providers.base_url<>excluded.base_url
			       OR providers.stream_usage<>excluded.stream_usage)`,
			newID(), name, kind, base, preset.AuthStyles[0], "client", boolInt(cp.StreamUsage),
			"off", "config", 1, nil, now, now)
		if err != nil {
			return touched, wrap("import provider "+name, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			touched++
		}
	}
	if touched > 0 {
		if err := d.Audit(ctx, "config", "provider.import", "config.toml", map[string]any{"count": touched}); err != nil {
			return touched, err
		}
	}
	return touched, nil
}

// normalizeProvider applies defaults and validates every field. A provider that
// fails here never reaches the database.
func normalizeProvider(p Provider) (Provider, error) {
	p.Name = strings.TrimSpace(p.Name)
	if !config.ProviderName.MatchString(p.Name) {
		return Provider{}, invalid("name %q must match %s", p.Name, config.ProviderName.String())
	}
	p.Kind = strings.TrimSpace(p.Kind)
	preset, ok := provider.PresetFor(p.Kind)
	if !ok {
		return Provider{}, invalid("unknown kind %q (want one of %s)", p.Kind, strings.Join(kindList(), ", "))
	}
	fresh := p.CreatedAt == 0
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if p.BaseURL == "" && preset.DefaultBaseURL != "" {
		p.BaseURL = preset.DefaultBaseURL
	}
	if err := validBaseURL(p.BaseURL); err != nil {
		return Provider{}, err
	}
	if p.AuthStyle == "" {
		p.AuthStyle = preset.AuthStyles[0]
	}
	if !provider.ValidAuthStyle(p.AuthStyle) {
		return Provider{}, invalid("unknown auth_style %q", p.AuthStyle)
	}
	if p.AuthMode == "" {
		p.AuthMode = "managed"
	}
	if !provider.ValidAuthMode(p.AuthMode) {
		return Provider{}, invalid("auth_mode %q must be managed or client", p.AuthMode)
	}
	switch p.ReconcileMode {
	case "":
		p.ReconcileMode = "off"
	case "off", "gap_fill", "mirror":
	default:
		return Provider{}, invalid("reconcile_mode %q must be off, gap_fill or mirror", p.ReconcileMode)
	}
	switch p.Origin {
	case "":
		p.Origin = "ui"
	case "ui", "config":
	default:
		return Provider{}, invalid("origin %q must be ui or config", p.Origin)
	}
	if fresh {
		// A zero bool in a creation request means "not set", not "false".
		p.Enabled = true
		p.StreamUsage = true
	}
	return p, nil
}

// validBaseURL accepts only absolute http(s) URLs without embedded credentials,
// so a typo cannot silently send a key somewhere else.
func validBaseURL(raw string) error {
	if raw == "" {
		return invalid("base_url is required for this kind")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return invalid("base_url %q: %v", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return invalid("base_url %q must start with http:// or https://", raw)
	}
	if u.Host == "" {
		return invalid("base_url %q has no host", raw)
	}
	if u.User != nil {
		return invalid("base_url must not embed credentials")
	}
	return nil
}

func kindList() []string {
	presets := provider.Presets()
	out := make([]string, 0, len(presets))
	for _, p := range presets {
		out = append(out, p.Kind)
	}
	return out
}

func encodeExtra(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", invalid("extra fields: %v", err)
	}
	return string(b), nil
}
