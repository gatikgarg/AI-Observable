package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ai-observable/ai-observable/internal/control"
	"github.com/ai-observable/ai-observable/internal/pricing"
	"github.com/ai-observable/ai-observable/internal/provider"
)

// healthz keeps the pre-API-Manager contract: a bare {"ok":true}.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]bool{"ok": true})
}

// statusResp is the additive status object the CLI and diagnostics read.
type statusResp struct {
	OK                bool   `json:"ok"`
	Calls             int64  `json:"calls"`
	DBSizeBytes       int64  `json:"db_size_bytes"`
	ViewSizeBytes     int64  `json:"view_size_bytes"`
	LastSnapshotAt    int64  `json:"last_snapshot_at"`
	LastSnapshotError string `json:"last_snapshot_error,omitempty"`
	DroppedRecords    int64  `json:"dropped_records"`
	Providers         int    `json:"providers"`
	KeysOK            int    `json:"keys_ok"`
	KeysBad           int    `json:"keys_bad"`
	ControlDB         string `json:"control_db"`
	Vault             string `json:"vault"`
	CollectorEnabled  bool   `json:"collector_enabled"`
	GatewayModels     int64  `json:"gateway_models_requests"`
	Version           string `json:"version"`
}

// status reports what the process is actually doing without reading the log.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := statusResp{
		OK:               true,
		Calls:            s.cachedCalls(ctx),
		DroppedRecords:   s.dropped(),
		ControlDB:        filepath.Join(s.DataDir, "control.db"),
		Vault:            s.vaultState(),
		CollectorEnabled: s.Collector != nil,
		Version:          s.Version,
	}
	if s.GatewayModels != nil {
		out.GatewayModels = s.GatewayModels()
	}
	if out.Version == "" {
		out.Version = "dev"
	}
	out.DBSizeBytes = fileSize(filepath.Join(s.DataDir, "llmtap.db"))
	if s.Snap != nil {
		out.ViewSizeBytes = fileSize(s.Snap.Target())
		if !s.Snap.Last.IsZero() {
			out.LastSnapshotAt = s.Snap.Last.UnixMilli()
		}
		if s.Snap.LastErr != nil {
			out.LastSnapshotError = control.ScrubText(s.Snap.LastErr.Error())
		}
	}
	if s.Control != nil {
		providers, err := s.Control.ListProviders(ctx)
		if err != nil {
			fail(w, err)
			return
		}
		out.Providers = len(providers)
		for _, p := range providers {
			creds, cerr := s.Control.ListCredentials(ctx, p.ID)
			if cerr != nil {
				fail(w, cerr)
				return
			}
			for _, c := range creds {
				switch c.Status {
				case "ok":
					out.KeysOK++
				case "invalid", "insufficient_scope", "error", "rate_limited":
					out.KeysBad++
				}
			}
		}
	}
	ok(w, out)
}

// cachedCalls avoids a COUNT(*) scan on every /status poll.
func (s *Server) cachedCalls(ctx context.Context) int64 {
	s.countMu.Lock()
	defer s.countMu.Unlock()
	if !s.countAt.IsZero() && time.Since(s.countAt) < 5*time.Second {
		return s.countN
	}
	if s.Store == nil {
		return 0
	}
	n, err := s.Store.Count(ctx)
	if err != nil {
		return s.countN
	}
	s.countN, s.countAt = n, time.Now()
	return n
}

func (s *Server) dropped() int64 {
	if s.Store == nil {
		return 0
	}
	return s.Store.Dropped()
}

func (s *Server) vaultState() string {
	if s.Vault == nil || !s.Vault.Available() {
		return "locked"
	}
	return "unlocked"
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// presetView is the wizard's shape, not the provider package's: the UI wants
// the first auth style and mode flattened into one choice each.
type presetView struct {
	Kind            string   `json:"kind"`
	Label           string   `json:"label"`
	BaseURL         string   `json:"base_url"`
	AuthStyle       string   `json:"auth_style"`
	AuthMode        string   `json:"auth_mode"`
	AuthStyles      []string `json:"auth_styles"`
	DocsURL         string   `json:"docs_url"`
	PathPrefix      string   `json:"path_prefix"`
	Note            string   `json:"note"`
	NeedsDeployment bool     `json:"needs_deployment"`
	Capabilities    []string `json:"capabilities"`
}

func presetViews() []presetView {
	presets := provider.Presets()
	out := make([]presetView, 0, len(presets))
	for _, p := range presets {
		v := presetView{
			Kind: p.Kind, Label: p.Label, BaseURL: p.DefaultBaseURL,
			AuthStyles: p.AuthStyles, DocsURL: p.DocsURL, PathPrefix: p.PathPrefix,
			Note: p.Note, NeedsDeployment: p.NeedsDeployment,
			Capabilities: p.Capabilities.Labels(),
		}
		if len(p.AuthStyles) > 0 {
			v.AuthStyle = p.AuthStyles[0]
		}
		if len(p.AuthModes) > 0 {
			v.AuthMode = p.AuthModes[0]
		}
		out = append(out, v)
	}
	return out
}

type onboardingResp struct {
	Step             int          `json:"step"`
	HasProviders     bool         `json:"has_providers"`
	GrafanaURL       string       `json:"grafana_url"`
	Version          string       `json:"version"`
	Vault            string       `json:"vault"`
	CollectorEnabled bool         `json:"collector_enabled"`
	Presets          []presetView `json:"presets"`
}

// onboarding drives the first-run wizard and the degraded-mode banner.
func (s *Server) onboarding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := onboardingResp{
		Step: 1, GrafanaURL: s.effectiveConfig().Server.GrafanaURL, Version: s.Version,
		Vault: s.vaultState(), CollectorEnabled: s.Collector != nil, Presets: presetViews(),
	}
	if out.Version == "" {
		out.Version = "dev"
	}
	if s.Control != nil {
		providers, err := s.Control.ListProviders(ctx)
		if err != nil {
			fail(w, err)
			return
		}
		for _, p := range providers {
			if p.Origin != "config" {
				out.HasProviders = true
				break
			}
		}
	}
	if out.HasProviders {
		out.Step = 4
	}
	ok(w, out)
}

// listLogs returns the scrubbed ring buffer, newest first.
func (s *Server) listLogs(w http.ResponseWriter, r *http.Request) {
	limit := queryLimit(r, 200, 1000)
	level := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("level")))
	switch level {
	case "", "debug", "info", "warn", "error":
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "level must be debug, info, warn or error")
		return
	}
	ok(w, s.Logs.List(level, limit))
}

// takeSnapshot forces a view refresh. Snapshotter serialises concurrent calls.
func (s *Server) takeSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.Snap == nil {
		writeErr(w, http.StatusServiceUnavailable, "unsupported", "snapshotter is not running")
		return
	}
	if err := s.Snap.Take(); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "snapshot: "+control.ScrubText(err.Error()))
		return
	}
	s.Logs.Add("info", "snapshot taken on request", "view", s.Snap.Target())
	ok(w, map[string]int64{"snapshot_at": s.Snap.Last.UnixMilli()})
}

// queryLimit reads ?limit with a default and a hard ceiling, so a hand-written
// URL cannot ask the UI to render a million rows.
func queryLimit(r *http.Request, def, max int) int {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// decodeOptional reads a JSON body when one is present. Handlers that accept a
// body-less POST (validate, revoke, snapshot) call it so "{}" and an empty body
// behave the same instead of failing on io.EOF.
func decodeOptional(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	if !jsonContentType(r) {
		writeErr(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "bad_request", "body exceeds 1 MiB")
			return false
		}
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// pricePath is the price file the UI shows and reloads. An empty
// [pricing] file means ~/.llmtap/prices.json, which is DataDir/prices.json.
func (s *Server) pricePath() string {
	if f := strings.TrimSpace(s.effectiveConfig().Pricing.File); f != "" {
		if strings.HasPrefix(f, "~/") || strings.HasPrefix(f, `~\`) {
			if home, err := os.UserHomeDir(); err == nil {
				return filepath.Join(home, f[2:])
			}
		}
		return f
	}
	return filepath.Join(s.DataDir, "prices.json")
}

type pricingResp struct {
	File      string `json:"file"`
	Path      string `json:"path"`
	ModTimeMS int64  `json:"mod_time_ms"`
	Models    int    `json:"models"`
	Version   string `json:"version"`
	Source    string `json:"source"`
	LastError string `json:"last_error"`
}

func (s *Server) priceStatus() pricingResp {
	st := pricing.FileStatus()
	file := st.Path
	if strings.TrimSpace(file) == "" {
		file = s.pricePath()
	}
	source := "embedded prices.json"
	if !st.ModTime.IsZero() {
		source = "user price file"
	} else if st.Version != "" {
		source += " (" + st.Version + ")"
	}
	return pricingResp{File: file, Path: st.Path, ModTimeMS: st.ModTime.UnixMilli(),
		Models: st.Models, Version: st.Version, Source: source, LastError: st.LastError}
}

func (s *Server) pricingStatus(w http.ResponseWriter, r *http.Request) {
	ok(w, s.priceStatus())
}

// refreshPrices reloads the user price file and optionally re-pulls a catalog.
func (s *Server) refreshPrices(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProviderID string `json:"provider_id"`
	}
	if !decodeOptional(w, r, &body) {
		return
	}
	path := s.pricePath()
	if err := pricing.LoadFile(path); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "price file: "+control.ScrubText(err.Error()))
		return
	}
	if s.Control != nil {
		if err := s.Control.Audit(r.Context(), "ui", "pricing.reload", path, nil); err != nil {
			fail(w, err)
			return
		}
	}
	st := s.priceStatus()
	s.Logs.Add("info", "price file reloaded", "file", st.File, "models", strconv.Itoa(st.Models))
	s.queueCollect(r, body.ProviderID, "models", false)
	accepted(w, map[string]any{"accepted": true, "models": st.Models, "source": st.Source,
		"file": st.File, "last_error": st.LastError})
}
