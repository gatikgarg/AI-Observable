package admin

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/ai-observable/ai-observable/internal/provider"
)

// listProviderCredentials returns the safe view of every key for one provider.
func (s *Server) listProviderCredentials(w http.ResponseWriter, r *http.Request) {
	db := s.controlReady(w)
	if db == nil {
		return
	}
	p, err := db.GetProvider(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	rows, err := db.ListCredentials(r.Context(), p.ID)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]credentialView, 0, len(rows))
	for _, c := range rows {
		out = append(out, viewCredential(c))
	}
	ok(w, out)
}

// addCredential encrypts and stores a key. The plaintext is never echoed and
// never logged.
func (s *Server) addCredential(w http.ResponseWriter, r *http.Request) {
	db := s.controlReady(w)
	if db == nil {
		return
	}
	var body struct {
		Label  string `json:"label"`
		Secret string `json:"secret"`
		Scope  string `json:"scope"`
	}
	if !decode(w, r, &body) {
		return
	}
	secret := strings.TrimSpace(body.Secret)
	if secret == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "secret is required")
		return
	}
	ctx := s.uiCtx(r)
	p, err := db.GetProvider(ctx, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	c, err := db.AddCredential(ctx, p.ID, strings.TrimSpace(body.Label), strings.TrimSpace(body.Scope), secret, s.Vault)
	if err != nil {
		fail(w, err)
		return
	}
	s.invalidate(p.Name)
	s.Logs.Add("info", "key added", "provider", p.Name, "key", c.Masked())
	ok(w, viewCredential(c))
}

// probeResult is the answer shared by validate and test: a bad key is a 200.
type probeResult struct {
	OK           bool          `json:"ok"`
	Status       string        `json:"status"`
	Detail       string        `json:"detail,omitempty"`
	LatencyMS    int64         `json:"latency_ms"`
	Identity     *identityView `json:"identity,omitempty"`
	Capabilities []string      `json:"capabilities"`
}

func (s *Server) validateCredential(w http.ResponseWriter, r *http.Request) {
	db := s.controlReady(w)
	if db == nil {
		return
	}
	if !decodeOptional(w, r, &struct{}{}) {
		return
	}
	row, err := db.GetCredential(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	p, err := db.GetProvider(r.Context(), row.ProviderID)
	if err != nil {
		fail(w, err)
		return
	}
	adapter, err := provider.For(p.Kind)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "provider "+p.Name+": "+err.Error())
		return
	}
	secret, err := db.Secret(r.Context(), row.ID, s.Vault)
	if err != nil {
		fail(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	out := probeResult{Capabilities: labelsOrEmpty(adapter.Capabilities())}
	start := time.Now()
	identity, verr := adapter.Validate(ctx, row.Seam(secret), p.Seam())
	out.LatencyMS = time.Since(start).Milliseconds()
	if verr != nil {
		out.Status, out.Detail = authStatus(verr)
	} else {
		out.OK, out.Status, out.Identity = true, "ok", viewIdentity(identity)
	}
	// An unsupported adapter leaves the stored status alone instead of
	// overwriting a real verdict with "unverified".
	if out.Status != "unverified" {
		if err = db.SetCredentialStatus(s.uiCtx(r), row.ID, out.Status, out.Detail); err != nil {
			fail(w, err)
			return
		}
		s.invalidate(p.Name)
	}
	level := "info"
	if !out.OK {
		level = "warn"
	}
	s.Logs.Add(level, "key validated", "provider", p.Name, "key", row.Masked(), "status", out.Status)
	ok(w, out)
}

func (s *Server) revokeCredential(w http.ResponseWriter, r *http.Request) {
	db := s.controlReady(w)
	if db == nil {
		return
	}
	if !decodeOptional(w, r, &struct{}{}) {
		return
	}
	ctx := s.uiCtx(r)
	row, err := db.GetCredential(ctx, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if err = db.RevokeCredential(ctx, row.ID); err != nil {
		fail(w, err)
		return
	}
	name := row.ProviderID
	if p, perr := db.GetProvider(ctx, row.ProviderID); perr == nil {
		name = p.Name
	}
	s.invalidate(name)
	s.Logs.Add("warn", "key revoked", "provider", name, "key", row.Masked())
	ok(w, map[string]any{"revoked": true, "id": row.ID})
}

// revealCredential is the one route that returns plaintext. It requires
// [vault] allow_reveal, a loopback client and the guards ServeHTTP already
// applied, and it always writes an audit row.
func (s *Server) revealCredential(w http.ResponseWriter, r *http.Request) {
	db := s.controlReady(w)
	if db == nil {
		return
	}
	if !s.effectiveConfig().Vault.AllowReveal {
		writeErr(w, http.StatusForbidden, "forbidden",
			"revealing secrets is disabled; set [vault] allow_reveal = true in config.toml")
		return
	}
	if !isLoopbackAddr(r.RemoteAddr) {
		writeErr(w, http.StatusForbidden, "forbidden", "reveal is only allowed for loopback clients")
		return
	}
	ctx := s.uiCtx(r)
	row, err := db.GetCredential(ctx, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	secret, err := db.Secret(ctx, row.ID, s.Vault)
	if err != nil {
		fail(w, err)
		return
	}
	if err = db.Audit(ctx, "ui", "credential.reveal", row.ID, map[string]any{
		"provider_id": row.ProviderID, "key": row.Masked(),
	}); err != nil {
		fail(w, err)
		return
	}
	s.Logs.Add("warn", "key revealed", "provider_id", row.ProviderID, "key", row.Masked())
	w.Header().Set("Cache-Control", "no-store")
	ok(w, map[string]any{
		"id": row.ID, "provider_id": row.ProviderID, "label": row.Label,
		"masked": row.Masked(), "secret": secret,
	})
}

// keyView adds the provider name so the Keys tab renders without a join in the
// browser.
type keyView struct {
	credentialView
	Provider string `json:"provider"`
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	db := s.controlReady(w)
	if db == nil {
		return
	}
	ctx := r.Context()
	providers, err := db.ListProviders(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	out := []keyView{}
	for _, p := range providers {
		rows, cerr := db.ListCredentials(ctx, p.ID)
		if cerr != nil {
			fail(w, cerr)
			return
		}
		for _, c := range rows {
			out = append(out, keyView{credentialView: viewCredential(c), Provider: p.Name})
		}
	}
	ok(w, out)
}
