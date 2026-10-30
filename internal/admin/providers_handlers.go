package admin

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ai-observable/ai-observable/internal/control"
	"github.com/ai-observable/ai-observable/internal/provider"
)

// probeTimeout bounds one validation call. Adapters use the shared HTTP client
// with a 60s timeout; a UI click should not wait that long.
const probeTimeout = 20 * time.Second

// listProviders returns every row with its credentials and derived health, so
// the Providers page renders from one request.
func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) {
	db := s.controlReady(w)
	if db == nil {
		return
	}
	ctx := r.Context()
	rows, err := db.ListProviders(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]providerView, 0, len(rows))
	for _, p := range rows {
		creds, cerr := db.ListCredentials(ctx, p.ID)
		if cerr != nil {
			fail(w, cerr)
			return
		}
		out = append(out, s.viewProvider(p, creds))
	}
	ok(w, out)
}

// providerRequest is the wizard's create payload.
type providerRequest struct {
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`
	BaseURL       string            `json:"base_url"`
	AuthStyle     string            `json:"auth_style"`
	AuthMode      string            `json:"auth_mode"`
	ReconcileMode string            `json:"reconcile_mode"`
	StreamUsage   *bool             `json:"stream_usage"`
	Enabled       *bool             `json:"enabled"`
	Extra         map[string]string `json:"extra"`
	// Label and Secret let the simplified onboarding wizard create the first key
	// in the same request as the provider. Secret is never logged or echoed.
	Label  string `json:"label"`
	Secret string `json:"secret"`
}

func (s *Server) createProvider(w http.ResponseWriter, r *http.Request) {
	db := s.controlReady(w)
	if db == nil {
		return
	}
	var body providerRequest
	if !decode(w, r, &body) {
		return
	}
	ctx := s.uiCtx(r)
	p, err := db.CreateProvider(ctx, control.Provider{
		Name: body.Name, Kind: body.Kind, BaseURL: body.BaseURL,
		AuthStyle: body.AuthStyle, AuthMode: body.AuthMode,
		ReconcileMode: body.ReconcileMode, Extra: body.Extra,
	})
	if err != nil {
		fail(w, err)
		return
	}
	// normalizeProvider defaults a fresh row to enabled/stream_usage=true, so an
	// explicit false in the request has to be applied as a follow-up update.
	want := p
	if body.StreamUsage != nil {
		want.StreamUsage = *body.StreamUsage
	}
	if body.Enabled != nil {
		want.Enabled = *body.Enabled
	}
	if want.StreamUsage != p.StreamUsage || want.Enabled != p.Enabled {
		if p, err = db.UpdateProvider(ctx, want); err != nil {
			fail(w, err)
			return
		}
	}
	// The simplified wizard sends the first key with the provider. Sealing it in
	// the same request keeps onboarding to one round-trip and rolls the provider
	// back when the vault cannot store the secret, so no keyless row is left.
	if strings.TrimSpace(body.Secret) != "" {
		c, cerr := db.AddCredential(ctx, p.ID, body.Label, "", body.Secret, s.Vault)
		if cerr != nil {
			if derr := db.DeleteProvider(ctx, p.ID); derr != nil {
				s.Logs.Add("warn", "rollback after failed key add", "provider", p.Name,
					"error", control.ScrubText(derr.Error()))
			}
			fail(w, cerr)
			return
		}
		s.Logs.Add("info", "key added", "provider", p.Name, "label", c.Label)
	}
	s.invalidate("") // a new provider changes routing for every cached target
	s.Logs.Add("info", "provider created", "provider", p.Name, "kind", p.Kind, "auth_mode", p.AuthMode)
	creds, err := db.ListCredentials(ctx, p.ID)
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, s.viewProvider(p, creds))
}

// providerPatch is a partial update: a nil field is untouched and an empty
// extra map clears the stored extras.
type providerPatch struct {
	Name          *string           `json:"name"`
	Kind          *string           `json:"kind"`
	BaseURL       *string           `json:"base_url"`
	AuthStyle     *string           `json:"auth_style"`
	AuthMode      *string           `json:"auth_mode"`
	ReconcileMode *string           `json:"reconcile_mode"`
	StreamUsage   *bool             `json:"stream_usage"`
	Enabled       *bool             `json:"enabled"`
	Extra         map[string]string `json:"extra"`
}

// definitionFields lists the fields a config-imported provider refuses to
// change. Enabled is deliberately absent: muting an imported row is allowed.
func (b providerPatch) definitionFields() []string {
	var out []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"name", b.Name != nil}, {"kind", b.Kind != nil}, {"base_url", b.BaseURL != nil},
		{"auth_style", b.AuthStyle != nil}, {"auth_mode", b.AuthMode != nil},
		{"reconcile_mode", b.ReconcileMode != nil}, {"stream_usage", b.StreamUsage != nil},
		{"extra", b.Extra != nil},
	} {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

func (s *Server) patchProvider(w http.ResponseWriter, r *http.Request) {
	db := s.controlReady(w)
	if db == nil {
		return
	}
	var body providerPatch
	if !decodeOptional(w, r, &body) {
		return
	}
	ctx := s.uiCtx(r)
	cur, err := db.GetProvider(ctx, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if cur.ReadOnly() {
		if fields := body.definitionFields(); len(fields) > 0 {
			writeErr(w, http.StatusBadRequest, "bad_request",
				"provider "+cur.Name+" is imported from config.toml; only enabled can be changed (asked for "+strings.Join(fields, ", ")+")")
			return
		}
		if body.Enabled == nil {
			ok(w, s.viewProvider(cur, nil))
			return
		}
		updated, serr := db.SetProviderEnabled(ctx, cur.ID, *body.Enabled)
		if serr != nil {
			fail(w, serr)
			return
		}
		s.invalidate(updated.Name)
		s.Logs.Add("info", "provider enabled changed", "provider", updated.Name,
			"enabled", strconv.FormatBool(updated.Enabled))
		creds, cerr := db.ListCredentials(ctx, updated.ID)
		if cerr != nil {
			fail(w, cerr)
			return
		}
		ok(w, s.viewProvider(updated, creds))
		return
	}
	next := cur
	if body.Name != nil {
		next.Name = *body.Name
	}
	if body.Kind != nil {
		next.Kind = *body.Kind
	}
	if body.BaseURL != nil {
		next.BaseURL = *body.BaseURL
	}
	if body.AuthStyle != nil {
		next.AuthStyle = *body.AuthStyle
	}
	if body.AuthMode != nil {
		next.AuthMode = *body.AuthMode
	}
	if body.ReconcileMode != nil {
		next.ReconcileMode = *body.ReconcileMode
	}
	if body.StreamUsage != nil {
		next.StreamUsage = *body.StreamUsage
	}
	if body.Enabled != nil {
		next.Enabled = *body.Enabled
	}
	if body.Extra != nil {
		next.Extra = body.Extra
	}
	updated, err := db.UpdateProvider(ctx, next)
	if err != nil {
		fail(w, err)
		return
	}
	s.invalidate(cur.Name)
	if updated.Name != cur.Name {
		s.invalidate(updated.Name)
	}
	s.Logs.Add("info", "provider updated", "provider", updated.Name, "enabled", strconv.FormatBool(updated.Enabled))
	creds, err := db.ListCredentials(ctx, updated.ID)
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, s.viewProvider(updated, creds))
}

func (s *Server) deleteProvider(w http.ResponseWriter, r *http.Request) {
	db := s.controlReady(w)
	if db == nil {
		return
	}
	ctx := s.uiCtx(r)
	p, err := db.GetProvider(ctx, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if err = db.DeleteProvider(ctx, p.ID); err != nil {
		fail(w, err)
		return
	}
	s.invalidate(p.Name)
	s.Logs.Add("warn", "provider deleted", "provider", p.Name, "id", p.ID)
	ok(w, map[string]any{"deleted": true, "id": p.ID, "name": p.Name})
}

// testProvider spends one cheap call to prove the stored key works. A failed
// probe is a 200 with ok=false: the credential is bad, not the request.
func (s *Server) testProvider(w http.ResponseWriter, r *http.Request) {
	db := s.controlReady(w)
	if db == nil {
		return
	}
	if !decodeOptional(w, r, &struct{}{}) {
		return
	}
	p, err := db.GetProvider(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	adapter, err := provider.For(p.Kind)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "provider "+p.Name+": "+err.Error())
		return
	}
	out := probeResult{Capabilities: labelsOrEmpty(adapter.Capabilities())}
	cred, credID, reason := s.probeCredential(r.Context(), db, p)
	if reason != "" {
		out.Status = reason
		if reason == "vault_locked" {
			out.Detail = "the vault is locked, so the stored key cannot be decrypted"
		} else {
			out.Detail = "no usable key is stored for this provider"
		}
		ok(w, out)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()
	start := time.Now()
	identity, verr := adapter.Validate(ctx, cred, p.Seam())
	out.LatencyMS = time.Since(start).Milliseconds()
	if verr != nil {
		out.Status, out.Detail = authStatus(verr)
		s.recordProbe(db, r, p, credID, out)
		ok(w, out)
		return
	}
	out.OK, out.Status, out.Identity = true, "ok", viewIdentity(identity)
	s.recordProbe(db, r, p, credID, out)
	ok(w, out)
}

// recordProbe persists the outcome and logs it. An unsupported adapter never
// overwrites a status a real probe established.
func (s *Server) recordProbe(db *control.DB, r *http.Request, p control.Provider, credID string, out probeResult) {
	if credID != "" && out.Status != "unverified" {
		if err := db.SetCredentialStatus(s.uiCtx(r), credID, out.Status, out.Detail); err != nil {
			s.Logs.Add("warn", "cannot record key status", "provider", p.Name, "error", control.ScrubText(err.Error()))
		} else {
			s.invalidate(p.Name)
		}
	}
	level := "info"
	if !out.OK {
		level = "warn"
	}
	s.Logs.Add(level, "provider test", "provider", p.Name, "status", out.Status,
		"ms", strconv.FormatInt(out.LatencyMS, 10))
}
