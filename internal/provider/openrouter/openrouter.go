// Package openrouter implements the OpenRouter provider family: key inspection,
// the model catalog, daily activity and prepaid credits. OpenRouter responses
// also carry usage.cost, which the generic normalizer already understands.
package openrouter

import (
	"context"
	"net/http"

	"github.com/ai-observable/ai-observable/internal/provider"
)

// Adapter implements provider.Adapter for OpenRouter.
type Adapter struct{ hc *http.Client }

// New returns an adapter using the shared provider client.
func New() *Adapter { return &Adapter{hc: provider.Client()} }

func init() { provider.Register(New()) }

// Kind returns the preset kind.
func (a *Adapter) Kind() string { return "openrouter" }

// Capabilities reports what OpenRouter exposes to a key.
func (a *Adapter) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		Models: true, Usage: true, Costs: true, Balance: true,
		NativeUsageInResponse: true,
	}
}

// ParseUsage normalizes a chat/completions payload or SSE stream, including the
// provider-reported usage.cost.
func (a *Adapter) ParseUsage(stream bool, contentType string, body []byte) (provider.Usage, error) {
	return provider.ParseGeneric(stream, contentType, body)
}

type keyResponse struct {
	Data struct {
		Label             string  `json:"label"`
		Limit             float64 `json:"limit"`
		Usage             float64 `json:"usage"`
		IsFreeTier        bool    `json:"is_free_tier"`
		IsProvisioningKey bool    `json:"is_provisioning_key"`
	} `json:"data"`
}

// Validate inspects the key itself: the cheapest authenticated call available.
func (a *Adapter) Validate(ctx context.Context, c provider.Credential, p provider.Provider) (provider.Identity, error) {
	var out keyResponse
	if err := provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet,
		provider.JoinURL(p.BaseURL, "/v1/key"), nil, &out); err != nil {
		return provider.Identity{}, err
	}
	scope := "standard"
	if out.Data.IsProvisioningKey {
		scope = "provisioning"
	}
	return provider.Identity{KeyLabel: firstNonEmpty(out.Data.Label, c.Label), KeyScope: scope}, nil
}

type modelsResponse struct {
	Data []struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		ContextLength int64  `json:"context_length"`
		Pricing       struct {
			Prompt     string `json:"prompt"`
			Completion string `json:"completion"`
		} `json:"pricing"`
		TopProvider struct {
			ContextLength int64 `json:"context_length"`
		} `json:"top_provider"`
	} `json:"data"`
}

// ListModels returns the model catalog with context windows.
func (a *Adapter) ListModels(ctx context.Context, c provider.Credential, p provider.Provider) ([]provider.ModelInfo, error) {
	var out modelsResponse
	if err := provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet,
		provider.JoinURL(p.BaseURL, "/v1/models"), nil, &out); err != nil {
		return nil, err
	}
	infos := make([]provider.ModelInfo, 0, len(out.Data))
	for _, m := range out.Data {
		ctxMax := m.ContextLength
		if ctxMax == 0 {
			ctxMax = m.TopProvider.ContextLength
		}
		infos = append(infos, provider.ModelInfo{ID: m.ID, DisplayName: firstNonEmpty(m.Name, m.ID), ContextMax: ctxMax})
	}
	return infos, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
