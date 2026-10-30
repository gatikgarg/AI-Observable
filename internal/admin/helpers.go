package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ai-observable/ai-observable/internal/control"
	"github.com/ai-observable/ai-observable/internal/provider"
	"github.com/ai-observable/ai-observable/internal/store"
)

// controlReady and storeReady keep the degraded-mode contract: a route whose
// dependency is missing answers 503 instead of panicking or returning zeros
// that look like real measurements.
func (s *Server) controlReady(w http.ResponseWriter) *control.DB {
	if s.Control == nil {
		writeErr(w, http.StatusServiceUnavailable, "unsupported", "control database is not open")
		return nil
	}
	return s.Control
}

func (s *Server) storeReady(w http.ResponseWriter) *store.Store {
	if s.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "unsupported", "analytics database is not open")
		return nil
	}
	return s.Store
}

// uiCtx labels every audit row this package writes. control defaults to
// "system", which would hide who changed a key.
func (s *Server) uiCtx(r *http.Request) context.Context {
	return control.WithActor(r.Context(), "ui")
}

// invalidate drops the proxy's cached secret for one provider so a new or
// revoked key takes effect without restarting the daemon.
func (s *Server) invalidate(name string) {
	if s.Invalidate != nil {
		s.Invalidate(strings.TrimSpace(name))
	}
}

// queryWindow reads from/to (epoch milliseconds, both optional). The default
// range is `def` ending now, which is what the UI's 24h/7d/30d tabs rely on.
func queryWindow(r *http.Request, def time.Duration) (from, to int64, err error) {
	q := r.URL.Query()
	to = time.Now().UnixMilli()
	if raw := strings.TrimSpace(q.Get("to")); raw != "" {
		v, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || v <= 0 {
			return 0, 0, fmt.Errorf("to must be epoch milliseconds")
		}
		to = v
	}
	from = to - def.Milliseconds()
	if raw := strings.TrimSpace(q.Get("from")); raw != "" {
		v, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || v < 0 {
			return 0, 0, fmt.Errorf("from must be epoch milliseconds")
		}
		from = v
	}
	if from > to {
		return 0, 0, fmt.Errorf("from must be <= to")
	}
	return from, to, nil
}

// queryString is a trimmed query parameter, "" when absent.
func queryString(r *http.Request, key string) string {
	return strings.TrimSpace(r.URL.Query().Get(key))
}

// credentialView is the only shape credentials leave this package in: Masked is
// derived from the stored prefix/last4 and the ciphertext never travels.
type credentialView struct {
	control.Credential
	Masked string `json:"masked"`
}

func viewCredential(c control.Credential) credentialView {
	return credentialView{Credential: c, Masked: c.Masked()}
}

// providerView is the row plus the two derived pieces the UI draws.
type providerView struct {
	control.Provider
	Capabilities []string         `json:"capabilities"`
	Credentials  []credentialView `json:"credentials"`
	Health       string           `json:"health"`
}

func (s *Server) viewProvider(p control.Provider, creds []control.Credential) providerView {
	out := providerView{
		Provider:     p,
		Capabilities: labelsOrEmpty(provider.CapabilitiesFor(p.Kind)),
		Credentials:  make([]credentialView, 0, len(creds)),
	}
	for _, c := range creds {
		out.Credentials = append(out.Credentials, viewCredential(c))
	}
	vaultOpen := s.Vault != nil && s.Vault.Available()
	out.Health = providerHealth(p, creds, vaultOpen)
	return out
}

// providerHealth is a one-word summary the UI can colour without guessing.
func providerHealth(p control.Provider, creds []control.Credential, vaultOpen bool) string {
	if !p.Enabled {
		return "disabled"
	}
	if p.AuthMode == "client" {
		return "client"
	}
	if !vaultOpen {
		return "locked"
	}
	if len(creds) == 0 {
		return "missing_key"
	}
	var live, sawOK, sawBad bool
	for _, c := range creds {
		if c.Revoked() {
			continue
		}
		live = true
		switch c.Status {
		case "ok":
			sawOK = true
		case "invalid", "insufficient_scope", "error", "rate_limited":
			sawBad = true
		}
	}
	switch {
	case !live:
		return "revoked"
	case sawOK:
		return "ok"
	case sawBad:
		return "error"
	default:
		return "unverified"
	}
}

// labelsOrEmpty keeps JSON arrays stable: nil would serialize as null and the
// UI would render a broken chip row.
func labelsOrEmpty(c provider.Capabilities) []string {
	if out := c.Labels(); out != nil {
		return out
	}
	return []string{}
}

// authStatus maps an adapter error onto the credential status vocabulary
// control.SetCredentialStatus accepts, so a failed probe is durable.
func authStatus(err error) (status, detail string) {
	detail = control.ScrubText(err.Error())
	switch {
	case errors.Is(err, provider.ErrUnsupported):
		return "unverified", "this provider kind has no validation call"
	case provider.StatusOf(err) == http.StatusUnauthorized:
		return "invalid", detail
	case provider.StatusOf(err) == http.StatusForbidden:
		return "insufficient_scope", detail
	case provider.StatusOf(err) == http.StatusTooManyRequests:
		return "rate_limited", detail
	default:
		return "error", detail
	}
}

// probeCredential picks the key a test should use. A locked vault or a provider
// with no usable key is a test result, not a transport error, so the caller
// turns the returned reason into a readable status.
func (s *Server) probeCredential(ctx context.Context, db *control.DB, p control.Provider) (provider.Credential, string, string) {
	if s.Vault == nil || !s.Vault.Available() {
		return provider.Credential{}, "", "vault_locked"
	}
	secret, credID, err := db.ActiveSecretFor(ctx, p.ID, s.Vault)
	if err != nil {
		if errors.Is(err, control.ErrLocked) {
			return provider.Credential{}, "", "vault_locked"
		}
		return provider.Credential{}, "", "no_credential"
	}
	row, err := db.GetCredential(ctx, credID)
	if err != nil {
		// The secret is usable even if the metadata read failed; the adapter
		// only needs the plaintext and a label.
		return provider.Credential{Secret: secret, ID: credID}, credID, ""
	}
	return row.Seam(secret), credID, ""
}

// identityView flattens provider.Identity for JSON.
type identityView struct {
	Account  string `json:"account,omitempty"`
	Org      string `json:"org,omitempty"`
	KeyLabel string `json:"key_label,omitempty"`
	KeyScope string `json:"key_scope,omitempty"`
}

func viewIdentity(i provider.Identity) *identityView {
	out := identityView{Account: i.Account, Org: i.Org, KeyLabel: i.KeyLabel, KeyScope: i.KeyScope}
	if out == (identityView{}) {
		return nil
	}
	return &out
}
