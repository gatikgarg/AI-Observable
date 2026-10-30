// Package admin serves the API Manager UI and its JSON API on the admin
// listener. Every route is same-origin only: Host allowlist, Origin check,
// CSRF header on mutations, optional bearer token, 1 MiB body cap.
package admin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ai-observable/ai-observable/internal/collector"
	"github.com/ai-observable/ai-observable/internal/config"
	"github.com/ai-observable/ai-observable/internal/control"
	"github.com/ai-observable/ai-observable/internal/integration"
	"github.com/ai-observable/ai-observable/internal/snapshot"
	"github.com/ai-observable/ai-observable/internal/store"
	"github.com/ai-observable/ai-observable/internal/vault"
)

// MaxBody caps every JSON body; previews and exports are streamed, not buffered.
const MaxBody = 1 << 20

// Deps is everything the manager needs. Nil pieces degrade one route, never the
// listener: a locked vault still serves the Providers page with "locked".
type Deps struct {
	Config        config.Config
	Store         *store.Store
	Snap          *snapshot.Snapshotter
	Control       *control.DB
	Vault         *vault.Vault
	Collector     *collector.Collector
	Integrations  *integration.Service
	GatewayModels func() int64
	// Invalidate drops the proxy's cached secret for a provider ("" = all).
	Invalidate func(provider string)
	DataDir    string
	Version    string
	Logs       *Ring
}

// Server is the admin handler.
type Server struct {
	Deps
	token string
	csrf  string
	mux   *http.ServeMux

	// cfg is the effective configuration: Deps.Config plus whatever a
	// PATCH /api/config has already applied in this process.
	cfgMu sync.RWMutex
	cfg   config.Config

	// countN caches the calls count so /status does not scan on every poll.
	countMu sync.Mutex
	countN  int64
	countAt time.Time
}

// New builds the admin handler and registers every §8.1 route.
func New(d Deps) *Server {
	if d.Logs == nil {
		d.Logs = NewRing(200)
	}
	s := &Server{Deps: d, cfg: d.Config, token: ResolveToken(d.Config), csrf: newToken(), mux: http.NewServeMux()}
	s.routes()
	return s
}

// effectiveConfig is the configuration the handler should act on. Copying the
// struct is safe: the nested Providers map is only ever read.
func (s *Server) effectiveConfig() config.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *Server) setEffectiveConfig(c config.Config) {
	s.cfgMu.Lock()
	s.cfg = c
	s.cfgMu.Unlock()
}

// ResolveToken prefers LLMTAP_ADMIN_TOKEN over config.toml. Empty means
// loopback convenience mode.
func ResolveToken(cfg config.Config) string {
	if v := strings.TrimSpace(os.Getenv("LLMTAP_ADMIN_TOKEN")); v != "" {
		return v
	}
	return strings.TrimSpace(cfg.Admin.Token)
}

// CSRF exposes the per-process token to the shell and to tests.
func (s *Server) CSRF() string { return s.csrf }

func newToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Never expected on a supported platform; fall back to the clock rather
		// than panic on a read-only entropy source.
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /{$}", s.shell)
	m.HandleFunc("GET /assets/{name}", s.asset)
	m.HandleFunc("GET /healthz", s.healthz)
	m.HandleFunc("GET /status", s.status)
	m.HandleFunc("GET /api/onboarding", s.onboarding)

	m.HandleFunc("GET /api/providers", s.listProviders)
	m.HandleFunc("POST /api/providers", s.createProvider)
	m.HandleFunc("PATCH /api/providers/{id}", s.patchProvider)
	m.HandleFunc("DELETE /api/providers/{id}", s.deleteProvider)
	m.HandleFunc("POST /api/providers/{id}/test", s.testProvider)
	m.HandleFunc("GET /api/providers/{id}/credentials", s.listProviderCredentials)
	m.HandleFunc("POST /api/providers/{id}/credentials", s.addCredential)

	m.HandleFunc("POST /api/credentials/{id}/validate", s.validateCredential)
	m.HandleFunc("POST /api/credentials/{id}/revoke", s.revokeCredential)
	m.HandleFunc("GET /api/credentials/{id}/reveal", s.revealCredential)
	m.HandleFunc("GET /api/keys", s.listKeys)

	m.HandleFunc("GET /api/collector/jobs", s.listJobs)
	m.HandleFunc("PATCH /api/collector/jobs/{id}", s.patchJob)
	m.HandleFunc("POST /api/collector/run", s.runCollector)
	m.HandleFunc("GET /api/collector/runs", s.collectorRuns)

	m.HandleFunc("GET /api/usage/summary", s.usageSummary)
	m.HandleFunc("GET /api/usage/buckets", s.usageBuckets)
	m.HandleFunc("GET /api/costs/summary", s.costsSummary)
	m.HandleFunc("GET /api/models", s.listModels)
	m.HandleFunc("POST /api/prices/refresh", s.refreshPrices)
	m.HandleFunc("GET /api/sessions", s.listSessions)
	m.HandleFunc("GET /api/calls/recent", s.recentCalls)
	m.HandleFunc("GET /api/pricing/status", s.pricingStatus)
	m.HandleFunc("GET /api/export/calls.csv", s.exportCalls)

	m.HandleFunc("GET /api/agents", s.agentCatalog)
	m.HandleFunc("GET /api/agents/{agent}/compatible-providers", s.compatibleProviders)
	m.HandleFunc("POST /api/integrations", s.createIntegration)
	m.HandleFunc("GET /api/integrations", s.listIntegrations)
	m.HandleFunc("GET /api/integrations/{id}", s.getIntegration)
	m.HandleFunc("POST /api/integrations/{id}/apply", s.applyIntegration)
	m.HandleFunc("POST /api/integrations/{id}/verify", s.verifyIntegration)
	m.HandleFunc("POST /api/integrations/{id}/rotate-token", s.rotateIntegrationToken)
	m.HandleFunc("POST /api/integrations/{id}/rollback", s.rollbackIntegration)
	m.HandleFunc("DELETE /api/integrations/{id}", s.deleteIntegration)
	m.HandleFunc("GET /api/integrations/{id}/activity", s.integrationActivity)

	m.HandleFunc("GET /api/config", s.getConfig)
	m.HandleFunc("PATCH /api/config", s.patchConfig)
	m.HandleFunc("POST /api/snapshot", s.takeSnapshot)
	m.HandleFunc("GET /api/logs", s.listLogs)
}

// ServeHTTP applies the guards in the order the plan lists them: Host, token,
// Origin, CSRF, payload cap, request id.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := newToken()[:12]
	w.Header().Set("X-Request-Id", id)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !hostAllowed(r.Host, s.effectiveConfig()) {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid Host header")
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="llmtap"`)
		writeErr(w, http.StatusUnauthorized, "unauthorized", "admin token required")
		return
	}
	if mutating(r.Method) {
		if !sameOrigin(r) {
			writeErr(w, http.StatusForbidden, "forbidden", "cross-origin request rejected")
			return
		}
		if r.Header.Get("X-LLMTAP-CSRF") != s.csrf {
			writeErr(w, http.StatusForbidden, "forbidden", "missing or stale CSRF token")
			return
		}
		if r.Body != nil && r.ContentLength != 0 && !jsonContentType(r) {
			writeErr(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
			return
		}
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Cache-Control", "no-store")
	}
	s.mux.ServeHTTP(w, r)
}

func mutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// authorized accepts a bearer token (or X-LLMTAP-Token). With no token
// configured, loopback clients are trusted; anything else is rejected.
func (s *Server) authorized(r *http.Request) bool {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	gatewaySupplied := strings.HasPrefix(auth, "obs_gw_") ||
		strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")), "obs_gw_") ||
		strings.HasPrefix(strings.TrimSpace(r.Header.Get("x-api-key")), "obs_gw_")
	if gatewaySupplied {
		return false
	}
	if s.token == "" {
		return isLoopbackAddr(r.RemoteAddr)
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if got == "" {
		got = strings.TrimSpace(r.Header.Get("X-LLMTAP-Token"))
	}
	return subtleEqual(got, s.token)
}

// sameOrigin rejects a browser-originated cross-site request. A missing Origin
// means a non-browser client (curl, tests) and is allowed.
func sameOrigin(r *http.Request) bool {
	if o := strings.TrimSpace(r.Header.Get("Origin")); o != "" {
		u, err := url.Parse(o)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	}
	switch site := r.Header.Get("Sec-Fetch-Site"); site {
	case "", "same-origin", "none":
		return true
	default:
		return false
	}
}

func hostAllowed(host string, cfg config.Config) bool {
	h := host
	if only, _, err := net.SplitHostPort(host); err == nil {
		h = only
	}
	h = strings.Trim(strings.ToLower(h), "[]")
	switch h {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	want := cfg.Server.AdminAddr
	if only, _, err := net.SplitHostPort(want); err == nil {
		want = only
	}
	return h != "" && strings.EqualFold(h, strings.Trim(want, "[]"))
}

func isLoopbackAddr(addr string) bool {
	host := addr
	if only, _, err := net.SplitHostPort(addr); err == nil {
		host = only
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// subtleEqual is a constant-time compare that also hides length differences.
func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// apiErr is the documented error envelope.
type apiErr struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Field   string `json:"field,omitempty"`
	} `json:"error"`
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	var e apiErr
	e.Error.Code, e.Error.Message = code, message
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e)
}

// fail maps a control/vault error onto the documented envelope.
func fail(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		return
	case errors.Is(err, control.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, control.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, control.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
	case errors.Is(err, control.ErrLocked):
		writeErr(w, http.StatusForbidden, "forbidden", "vault is locked: "+err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func ok(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

func accepted(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(v)
}

// decode reads one JSON body with the 1 MiB cap applied by ServeHTTP and
// rejects unknown fields so a typo cannot silently become a no-op.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if !jsonContentType(r) {
		writeErr(w, http.StatusUnsupportedMediaType, "bad_request", "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func jsonContentType(r *http.Request) bool {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	return ct == "application/json"
}
