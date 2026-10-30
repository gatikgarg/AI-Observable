package admin

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/ai-observable/ai-observable/internal/config"
)

// configView is the Settings tab's view of the effective config. Secrets are
// never included: the admin token and the vault key are masked, not echoed.
type configView struct {
	ProxyAddr       string `json:"proxy_addr"`
	AdminAddr       string `json:"admin_addr"`
	GrafanaURL      string `json:"grafana_url"`
	OpenOnStart     bool   `json:"open_on_start"`
	CaptureBodies   string `json:"capture_bodies"`
	MaxBodyBytes    int    `json:"max_body_bytes"`
	RetentionDays   int    `json:"retention_days"`
	SnapshotSeconds int    `json:"snapshot_interval_seconds"`
	CollectorOn     bool   `json:"collector_enabled"`
	VaultMode       string `json:"vault_mode"`
	VaultState      string `json:"vault"`
	AllowReveal     bool   `json:"allow_reveal"`
	PriceFile       string `json:"price_file"`
	PricingHours    int    `json:"pricing_refresh_hours"`
	RequireToken    bool   `json:"require_token"`
	TokenSet        bool   `json:"token_set"`
	LastSnapshotMS  int64  `json:"last_snapshot_at"`
	DataDir         string `json:"data_dir"`
	ConfigFile      string `json:"config_file"`
	Version         string `json:"version"`
	RestartRequired bool   `json:"restart_required"`
	Note            string `json:"note,omitempty"`
}

// viewConfig renders the effective configuration.
func (s *Server) viewConfig(r *http.Request) configView {
	c := s.effectiveConfig()
	out := configView{
		ProxyAddr: c.Server.ProxyAddr, AdminAddr: c.Server.AdminAddr,
		GrafanaURL: c.Server.GrafanaURL, OpenOnStart: c.Server.OpenOnStart,
		CaptureBodies: c.Capture.Bodies, MaxBodyBytes: c.Capture.MaxBodyBytes,
		RetentionDays: c.Capture.RetentionDays, SnapshotSeconds: c.Snapshot.IntervalSeconds,
		CollectorOn: c.Collector.Enabled, VaultMode: c.Vault.Mode,
		VaultState: s.vaultState(), AllowReveal: c.Vault.AllowReveal,
		PriceFile: s.pricePath(), PricingHours: c.Pricing.RefreshHours,
		RequireToken: strings.TrimSpace(s.token) != "", TokenSet: strings.TrimSpace(s.token) != "",
		DataDir: s.DataDir, ConfigFile: config.Path(s.DataDir), Version: s.Version,
	}
	if out.Version == "" {
		out.Version = "dev"
	}
	if s.Snap != nil && !s.Snap.Last.IsZero() {
		out.LastSnapshotMS = s.Snap.Last.UnixMilli()
	}
	return out
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	ok(w, s.viewConfig(r))
}

// patchConfig accepts only the three keys the UI edits. Everything else in
// config.toml is owned by the user, so this route never rewrites the file; it
// changes the running configuration and says plainly that the proxy keeps its
// old capture settings until the process restarts.
func (s *Server) patchConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CaptureBodies *string `json:"capture_bodies"`
		RetentionDays *int    `json:"retention_days"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.CaptureBodies == nil && body.RetentionDays == nil {
		writeErr(w, http.StatusBadRequest, "bad_request",
			"send at least one of capture_bodies, retention_days")
		return
	}
	c := s.effectiveConfig()
	changed := []string{}
	if body.CaptureBodies != nil {
		bodies := strings.ToLower(strings.TrimSpace(*body.CaptureBodies))
		if bodies != "none" && bodies != "truncated" && bodies != "full" {
			writeErr(w, http.StatusBadRequest, "bad_request",
				"capture_bodies must be none, truncated or full")
			return
		}
		c.Capture.Bodies = bodies
		changed = append(changed, "capture.bodies="+bodies)
	}
	if body.RetentionDays != nil {
		days := *body.RetentionDays
		if days < 0 || days > 3650 {
			writeErr(w, http.StatusBadRequest, "bad_request", "retention_days must be between 0 and 3650")
			return
		}
		c.Capture.RetentionDays = days
		changed = append(changed, "capture.retention_days="+strconv.Itoa(days))
	}
	s.setEffectiveConfig(c)
	if s.Control != nil {
		if err := s.Control.Audit(r.Context(), "ui", "config.update", "config", map[string]any{
			"changed": changed,
		}); err != nil {
			fail(w, err)
			return
		}
	}
	s.Logs.Add("info", "config updated", "changed", strings.Join(changed, " "))
	out := s.viewConfig(r)
	if body.CaptureBodies != nil || body.RetentionDays != nil {
		out.RestartRequired = true
		out.Note = "capture and retention settings are read by the proxy and the daily " +
			"sweep at start-up; restart llmtap to apply them. Values are not written to config.toml."
	}
	ok(w, out)
}
