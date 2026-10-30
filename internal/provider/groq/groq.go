// Package groq implements the Groq provider family. Groq publishes a model
// catalog but no usage or cost history API, so the adapter says so instead of
// pretending.
package groq

import (
	"context"
	"net/http"

	"github.com/ai-observable/ai-observable/internal/provider"
)

// Adapter implements provider.Adapter for Groq.
type Adapter struct{ hc *http.Client }

// New returns an adapter using the shared provider client.
func New() *Adapter { return &Adapter{hc: provider.Client()} }

func init() { provider.Register(New()) }

// Kind returns the preset kind.
func (a *Adapter) Kind() string { return "groq" }

// Capabilities reports what Groq exposes to a key.
func (a *Adapter) Capabilities() provider.Capabilities {
	return provider.Capabilities{Models: true}
}

// ParseUsage normalizes a chat/completions payload or SSE stream.
func (a *Adapter) ParseUsage(stream bool, contentType string, body []byte) (provider.Usage, error) {
	return provider.ParseGeneric(stream, contentType, body)
}

type modelsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by"`
		Context int64  `json:"context_window"`
	} `json:"data"`
}

// Validate proves the key works with the cheapest call available.
func (a *Adapter) Validate(ctx context.Context, c provider.Credential, p provider.Provider) (provider.Identity, error) {
	var out modelsResponse
	if err := provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet,
		provider.JoinURL(p.BaseURL, "/v1/models"), nil, &out); err != nil {
		return provider.Identity{}, err
	}
	return provider.Identity{KeyLabel: c.Label, KeyScope: "project"}, nil
}

// ListModels returns the model catalog.
func (a *Adapter) ListModels(ctx context.Context, c provider.Credential, p provider.Provider) ([]provider.ModelInfo, error) {
	var out modelsResponse
	if err := provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet,
		provider.JoinURL(p.BaseURL, "/v1/models"), nil, &out); err != nil {
		return nil, err
	}
	infos := make([]provider.ModelInfo, 0, len(out.Data))
	for _, m := range out.Data {
		infos = append(infos, provider.ModelInfo{ID: m.ID, DisplayName: m.ID, ContextMax: m.Context})
	}
	return infos, nil
}

// Usage is unsupported: Groq has no usage history endpoint.
func (a *Adapter) Usage(context.Context, provider.Credential, provider.Provider, provider.Window) ([]provider.UsageBucket, error) {
	return nil, provider.ErrUnsupported
}

// Costs is unsupported: Groq has no cost history endpoint.
func (a *Adapter) Costs(context.Context, provider.Credential, provider.Provider, provider.Window) ([]provider.CostBucket, error) {
	return nil, provider.ErrUnsupported
}

// Balance is unsupported: Groq has no prepaid credit endpoint.
func (a *Adapter) Balance(context.Context, provider.Credential, provider.Provider) (*provider.Balance, error) {
	return nil, provider.ErrUnsupported
}
