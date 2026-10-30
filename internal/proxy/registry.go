package proxy

import (
	"context"
	"errors"
	"net/http"

	"github.com/ai-observable/ai-observable/internal/provider"
)

var (
	// ErrUnknownProvider is returned when neither the control DB nor config.toml
	// knows the requested path segment.
	ErrUnknownProvider = errors.New("proxy: unknown provider")
	// ErrNoCredential means auth_mode=managed but no usable key is stored.
	ErrNoCredential = errors.New("proxy: no active credential for provider")
	// ErrVaultLocked means auth_mode=managed and the vault cannot unseal.
	ErrVaultLocked = errors.New("proxy: vault is locked")
	// ErrDisabled means the provider exists but the manager has it switched off.
	ErrDisabled = errors.New("proxy: provider is disabled")
)

// Target is one resolved provider plus its decrypted secret for a single
// request. It carries no database handle so it can be cached freely.
type Target struct {
	Name          string
	ID            string
	Kind          string
	Upstream      string
	AuthStyle     string
	AuthMode      string
	StreamUsage   bool
	Extra         map[string]string
	Secret        string
	CredentialID  string
	Fingerprint   string
	IntegrationID string
	AgentType     string
	ProjectLabel  string
	CaptureMode   string
	RequestBytes  int64
}

// Managed reports whether the proxy replaces the client's credentials.
func (t Target) Managed() bool { return t.AuthMode == "managed" }

// Resolver is the seam the proxy uses to look providers up per request. The
// control-DB implementation lives in this package; tests use a fake.
type Resolver interface {
	Target(ctx context.Context, name string) (Target, error)
}

// authHeaders are the headers a managed request replaces with the stored key.
var authHeaders = []string{"Authorization", "x-api-key", "api-key", "x-goog-api-key"}

// applyAuth injects the stored secret according to the provider's auth_style.
// A managed request never leaks the client's placeholder credentials upstream.
func applyAuth(req *http.Request, t Target) {
	if !t.Managed() {
		return
	}
	for _, h := range authHeaders {
		req.Header.Del(h)
	}
	switch t.AuthStyle {
	case "x-api-key":
		req.Header.Set("x-api-key", t.Secret)
	case "api-key":
		req.Header.Set("api-key", t.Secret)
	case "x-goog-api-key":
		req.Header.Set("x-goog-api-key", t.Secret)
	case "query-key":
		q := req.URL.Query()
		q.Set("key", t.Secret)
		req.URL.RawQuery = q.Encode()
	case "none":
		// local servers: nothing to add
	default: // bearer
		req.Header.Set("Authorization", "Bearer "+t.Secret)
	}
}

// adapterUsage normalizes a response body with the provider's own adapter,
// falling back to the generic parser for unknown kinds.
func adapterUsage(kind string, stream bool, contentType string, body []byte) provider.Usage {
	if a, err := provider.For(kind); err == nil {
		if u, perr := a.ParseUsage(stream, contentType, body); perr == nil && !u.Empty() {
			return u
		}
	}
	u, _ := provider.ParseGeneric(stream, contentType, body)
	return u
}
