package proxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ai-observable/ai-observable/internal/config"
	"github.com/ai-observable/ai-observable/internal/control"
	"github.com/ai-observable/ai-observable/internal/vault"
)

// ControlRegistry resolves providers from control.db, falling back to
// config.toml for rows nobody imported yet, and caches decrypted secrets for a
// short TTL so a hot proxy path does not unseal on every request.
type ControlRegistry struct {
	DB    *control.DB
	Vault *vault.Vault
	// Fallback keeps a pre-API-Manager config.toml usable without importing it.
	Fallback map[string]config.Provider
	TTL      time.Duration

	mu    sync.Mutex
	cache map[string]cachedSecret
}

type cachedSecret struct {
	secret, credID, fingerprint string
	expires                     time.Time
}

// NewControlRegistry wires the registry used by `llmtap start`.
func NewControlRegistry(db *control.DB, v *vault.Vault, cfg config.Config) *ControlRegistry {
	return &ControlRegistry{DB: db, Vault: v, Fallback: cfg.Providers, TTL: 30 * time.Second,
		cache: map[string]cachedSecret{}}
}

// Invalidate drops cached secrets for one provider (all when name is empty).
// The admin API calls it after a credential or provider change.
func (r *ControlRegistry) Invalidate(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name == "" {
		r.cache = map[string]cachedSecret{}
		return
	}
	for k := range r.cache {
		if strings.HasPrefix(k, name+"\x00") {
			delete(r.cache, k)
		}
	}
}

// Target resolves a path segment. Control-DB rows win over config.toml rows.
func (r *ControlRegistry) Target(ctx context.Context, name string) (Target, error) {
	if r == nil || r.DB == nil {
		return Target{}, ErrUnknownProvider
	}
	p, err := r.DB.GetProvider(ctx, name)
	switch {
	case err == nil:
		return r.targetFor(ctx, p)
	case errors.Is(err, control.ErrNotFound):
		// fall through to config.toml below
	default:
		return Target{}, fmt.Errorf("proxy: resolve %s: %w", name, err)
	}
	cp, ok := r.Fallback[name]
	if !ok {
		return Target{}, fmt.Errorf("%w: %s", ErrUnknownProvider, name)
	}
	return Target{Name: name, Kind: name, Upstream: cp.Upstream, AuthStyle: "bearer",
		AuthMode: "client", StreamUsage: cp.StreamUsage}, nil
}

func (r *ControlRegistry) targetFor(ctx context.Context, p control.Provider) (Target, error) {
	if !p.Enabled {
		return Target{}, fmt.Errorf("%w: %s", ErrDisabled, p.Name)
	}
	t := Target{Name: p.Name, ID: p.ID, Kind: p.Kind, Upstream: p.BaseURL, AuthStyle: p.AuthStyle,
		AuthMode: p.AuthMode, StreamUsage: p.StreamUsage, Extra: p.Extra}
	if p.AuthMode != "managed" {
		return t, nil // auth_mode=client: the client's own key wins, always
	}
	secret, credID, fingerprint, err := r.secretFor(ctx, p)
	if err != nil {
		return Target{}, err
	}
	t.Secret, t.CredentialID, t.Fingerprint = secret, credID, fingerprint
	return t, nil
}

func (r *ControlRegistry) secretFor(ctx context.Context, p control.Provider) (string, string, string, error) {
	key := p.ID + "\x00" + p.Name
	r.mu.Lock()
	c, ok := r.cache[key]
	r.mu.Unlock()
	if ok && time.Now().Before(c.expires) {
		return c.secret, c.credID, c.fingerprint, nil
	}
	secret, credID, err := r.DB.ActiveSecretFor(ctx, p.ID, r.Vault)
	if errors.Is(err, control.ErrLocked) {
		return "", "", "", fmt.Errorf("%w for %s", ErrVaultLocked, p.Name)
	}
	if errors.Is(err, control.ErrNotFound) || (err == nil && secret == "") {
		return "", "", "", fmt.Errorf("%w: %s", ErrNoCredential, p.Name)
	}
	if err != nil {
		return "", "", "", fmt.Errorf("proxy: credential %s: %w", p.Name, err)
	}
	cred, err := r.DB.GetCredential(ctx, credID)
	if err != nil {
		return "", "", "", fmt.Errorf("proxy: credential %s: %w", p.Name, err)
	}
	r.mu.Lock()
	r.cache[key] = cachedSecret{secret: secret, credID: credID, fingerprint: cred.Fingerprint,
		expires: time.Now().Add(r.ttl())}
	r.mu.Unlock()
	return secret, credID, cred.Fingerprint, nil
}

func (r *ControlRegistry) ttl() time.Duration {
	if r.TTL <= 0 {
		return 30 * time.Second
	}
	return r.TTL
}
