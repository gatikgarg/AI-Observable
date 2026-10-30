// Package openai implements the OpenAI provider family: models, organization
// usage and organization costs. Usage and costs need an Admin API key.
package openai

import (
	"context"
	"net/http"

	"github.com/ai-observable/ai-observable/internal/provider"
)

// maxPages bounds every paginated call; a runaway provider must not hang the
// collector.
const maxPages = 10

// Adapter implements provider.Adapter for OpenAI.
type Adapter struct{ hc *http.Client }

// New returns an adapter using the shared provider client.
func New() *Adapter { return &Adapter{hc: provider.Client()} }

func init() { provider.Register(New()) }

// Kind returns the preset kind.
func (a *Adapter) Kind() string { return "openai" }

// Capabilities reports what OpenAI exposes to a key.
func (a *Adapter) Capabilities() provider.Capabilities {
	return provider.Capabilities{Models: true, Usage: true, Costs: true}
}

// ParseUsage normalizes a chat/completions or responses payload.
func (a *Adapter) ParseUsage(stream bool, contentType string, body []byte) (provider.Usage, error) {
	return provider.ParseGeneric(stream, contentType, body)
}

type modelsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// Validate proves the key works with the cheapest call available.
func (a *Adapter) Validate(ctx context.Context, c provider.Credential, p provider.Provider) (provider.Identity, error) {
	var out modelsResponse
	if err := provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet, provider.JoinURL(p.BaseURL, "/v1/models?limit=1"), nil, &out); err != nil {
		return provider.Identity{}, err
	}
	return provider.Identity{KeyLabel: c.Label, KeyScope: "organization"}, nil
}

// ListModels returns the model catalog.
func (a *Adapter) ListModels(ctx context.Context, c provider.Credential, p provider.Provider) ([]provider.ModelInfo, error) {
	var out modelsResponse
	if err := provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet, provider.JoinURL(p.BaseURL, "/v1/models"), nil, &out); err != nil {
		return nil, err
	}
	infos := make([]provider.ModelInfo, 0, len(out.Data))
	for _, m := range out.Data {
		infos = append(infos, provider.ModelInfo{ID: m.ID, DisplayName: m.ID})
	}
	return infos, nil
}
