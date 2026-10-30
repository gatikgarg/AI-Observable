package control

import "github.com/ai-observable/ai-observable/internal/provider"

// Seam converts a control row into the provider package's own view. Keeping
// the conversion here means provider adapters never import control's storage
// shape, and the collector only sees seam types.
func (p Provider) Seam() provider.Provider {
	return provider.Provider{
		Kind:        p.Kind,
		BaseURL:     p.BaseURL,
		AuthStyle:   p.AuthStyle,
		AuthMode:    p.AuthMode,
		StreamUsage: p.StreamUsage,
		Extra:       p.Extra,
	}
}

// Seam attaches an in-memory plaintext secret. The secret is never stored on
// the Credential row.
func (c Credential) Seam(secret string) provider.Credential {
	return provider.Credential{Secret: secret, ID: c.ID, Label: c.Label, Scope: c.Scope}
}
