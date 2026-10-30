// Package gemini implements the Google Gemini provider family: the model
// catalog and native usageMetadata parsing. Per-key usage history needs Google
// Cloud billing, which this build does not read.
package gemini

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/ai-observable/ai-observable/internal/provider"
)

// Adapter implements provider.Adapter for Gemini.
type Adapter struct{ hc *http.Client }

// New returns an adapter using the shared provider client.
func New() *Adapter { return &Adapter{hc: provider.Client()} }

func init() { provider.Register(New()) }

// Kind returns the preset kind.
func (a *Adapter) Kind() string { return "gemini" }

// Capabilities reports what the Generative Language API exposes.
func (a *Adapter) Capabilities() provider.Capabilities {
	return provider.Capabilities{Models: true, NativeUsageInResponse: true}
}

// ParseUsage normalizes a generateContent payload or stream.
func (a *Adapter) ParseUsage(stream bool, contentType string, body []byte) (provider.Usage, error) {
	return provider.ParseGeneric(stream, contentType, body)
}

type modelsResponse struct {
	Models []struct {
		Name                       string   `json:"name"`
		DisplayName                string   `json:"displayName"`
		Description                string   `json:"description"`
		InputTokenLimit            int64    `json:"inputTokenLimit"`
		OutputTokenLimit           int64    `json:"outputTokenLimit"`
		SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
	} `json:"models"`
	NextPageToken string `json:"nextPageToken"`
}

// Validate proves the key works with the cheapest call available.
func (a *Adapter) Validate(ctx context.Context, c provider.Credential, p provider.Provider) (provider.Identity, error) {
	var out modelsResponse
	if err := provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet,
		provider.JoinURL(p.BaseURL, "/v1beta/models?pageSize=1"), nil, &out); err != nil {
		return provider.Identity{}, err
	}
	return provider.Identity{KeyLabel: c.Label, KeyScope: "project"}, nil
}

// ListModels returns the model catalog with context windows.
func (a *Adapter) ListModels(ctx context.Context, c provider.Credential, p provider.Provider) ([]provider.ModelInfo, error) {
	var out []provider.ModelInfo
	token := ""
	for i := 0; i < 10; i++ {
		q := url.Values{"pageSize": {"1000"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var resp modelsResponse
		if err := provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet,
			provider.WithQuery(provider.JoinURL(p.BaseURL, "/v1beta/models"), q), nil, &resp); err != nil {
			return nil, err
		}
		for _, m := range resp.Models {
			caps := map[string]bool{}
			for _, method := range m.SupportedGenerationMethods {
				caps[method] = true
			}
			out = append(out, provider.ModelInfo{
				ID:           strings.TrimPrefix(m.Name, "models/"),
				DisplayName:  firstNonEmpty(m.DisplayName, strings.TrimPrefix(m.Name, "models/")),
				ContextMax:   m.InputTokenLimit,
				Capabilities: caps,
			})
		}
		if resp.NextPageToken == "" {
			break
		}
		token = resp.NextPageToken
	}
	return out, nil
}

// Usage is unsupported: Gemini exposes no per-key token history endpoint.
func (a *Adapter) Usage(context.Context, provider.Credential, provider.Provider, provider.Window) ([]provider.UsageBucket, error) {
	return nil, provider.ErrUnsupported
}

// Costs is unsupported: spend lives in Google Cloud billing.
func (a *Adapter) Costs(context.Context, provider.Credential, provider.Provider, provider.Window) ([]provider.CostBucket, error) {
	return nil, provider.ErrUnsupported
}

// Balance is unsupported: Gemini API keys have no prepaid credit.
func (a *Adapter) Balance(context.Context, provider.Credential, provider.Provider) (*provider.Balance, error) {
	return nil, provider.ErrUnsupported
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
