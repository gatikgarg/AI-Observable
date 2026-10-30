// Package local implements the local and openai-compatible provider families:
// Ollama, LM Studio, vLLM and hosted gateways that speak the OpenAI schema.
// Nothing is billed, so usage, costs and balance are unsupported by design.
package local

import (
	"context"
	"net/http"

	"github.com/ai-observable/ai-observable/internal/provider"
)

// Adapter implements provider.Adapter for one local-family kind.
type Adapter struct {
	kind string
	hc   *http.Client
}

// New returns the "local" adapter.
func New() *Adapter { return &Adapter{kind: "local", hc: provider.Client()} }

// NewCompatible returns the "openai-compatible" adapter used for gateways.
func NewCompatible() *Adapter { return &Adapter{kind: "openai-compatible", hc: provider.Client()} }

func init() {
	provider.Register(New())
	provider.Register(NewCompatible())
}

// Kind returns the preset kind.
func (a *Adapter) Kind() string { return a.kind }

// Capabilities reports what a local endpoint exposes.
func (a *Adapter) Capabilities() provider.Capabilities {
	return provider.Capabilities{Models: true}
}

// ParseUsage normalizes a chat/completions payload or SSE stream. A server that
// reports no usage yields an empty Usage and a nil error.
func (a *Adapter) ParseUsage(stream bool, contentType string, body []byte) (provider.Usage, error) {
	return provider.ParseGeneric(stream, contentType, body)
}

type modelsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// Validate proves the endpoint answers by listing models.
func (a *Adapter) Validate(ctx context.Context, c provider.Credential, p provider.Provider) (provider.Identity, error) {
	var out modelsResponse
	if err := provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet,
		provider.JoinURL(p.BaseURL, "/v1/models"), nil, &out); err != nil {
		return provider.Identity{}, err
	}
	return provider.Identity{KeyLabel: c.Label, KeyScope: "local"}, nil
}

// ListModels returns the local catalog.
func (a *Adapter) ListModels(ctx context.Context, c provider.Credential, p provider.Provider) ([]provider.ModelInfo, error) {
	var out modelsResponse
	if err := provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet,
		provider.JoinURL(p.BaseURL, "/v1/models"), nil, &out); err != nil {
		return nil, err
	}
	infos := make([]provider.ModelInfo, 0, len(out.Data))
	for _, m := range out.Data {
		infos = append(infos, provider.ModelInfo{ID: m.ID, DisplayName: m.ID})
	}
	return infos, nil
}

// Usage is unsupported: local servers keep no billing history.
func (a *Adapter) Usage(context.Context, provider.Credential, provider.Provider, provider.Window) ([]provider.UsageBucket, error) {
	return nil, provider.ErrUnsupported
}

// Costs is unsupported: local calls cost nothing to the operator.
func (a *Adapter) Costs(context.Context, provider.Credential, provider.Provider, provider.Window) ([]provider.CostBucket, error) {
	return nil, provider.ErrUnsupported
}

// Balance is unsupported: there is no credit to report.
func (a *Adapter) Balance(context.Context, provider.Credential, provider.Provider) (*provider.Balance, error) {
	return nil, provider.ErrUnsupported
}
