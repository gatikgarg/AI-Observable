// Package azure implements the Azure OpenAI provider family: deployment
// discovery. Azure exposes no per-key usage or cost history to an API key, so
// those lanes report ErrUnsupported rather than guessing.
package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ai-observable/ai-observable/internal/provider"
)

// Adapter implements provider.Adapter for Azure OpenAI.
type Adapter struct{ hc *http.Client }

// New returns an adapter using the shared provider client.
func New() *Adapter { return &Adapter{hc: provider.Client()} }

func init() { provider.Register(New()) }

// Kind returns the preset kind.
func (a *Adapter) Kind() string { return "azure-openai" }

// Capabilities reports what Azure OpenAI exposes to a key.
func (a *Adapter) Capabilities() provider.Capabilities {
	return provider.Capabilities{}
}

// ParseUsage normalizes a chat/completions payload or SSE stream.
func (a *Adapter) ParseUsage(stream bool, contentType string, body []byte) (provider.Usage, error) {
	return provider.ParseGeneric(stream, contentType, body)
}

// apiVersion resolves the required api-version from the provider extras.
func apiVersion(p provider.Provider) (string, error) {
	for _, k := range []string{"api_version", "api-version"} {
		if v := strings.TrimSpace(p.Extra[k]); v != "" {
			return v, nil
		}
	}
	return "", errors.New("azure-openai: extra api_version is required")
}

// deployment resolves the optional deployment name.
func deployment(p provider.Provider) string { return strings.TrimSpace(p.Extra["deployment"]) }

type deploymentsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Owner   string `json:"owner"`
		Status  string `json:"status"`
		Created int64  `json:"created_at"`
	} `json:"data"`
	NextLink string `json:"nextLink"`
}

func (a *Adapter) listDeployments(ctx context.Context, c provider.Credential, p provider.Provider) ([]provider.ModelInfo, error) {
	ver, err := apiVersion(p)
	if err != nil {
		return nil, err
	}
	q := url.Values{"api-version": {ver}}
	endpoint := provider.WithQuery(provider.JoinURL(p.BaseURL, "/deployments"), q)
	var out []provider.ModelInfo
	var resp deploymentsResponse
	if err = provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet, endpoint, nil, &resp); err != nil {
		return nil, err
	}
	for _, d := range resp.Data {
		out = append(out, provider.ModelInfo{ID: d.ID, DisplayName: firstNonEmpty(d.Model, d.ID)})
	}
	return out, nil
}

// Validate proves the key and api-version work by listing deployments.
func (a *Adapter) Validate(ctx context.Context, c provider.Credential, p provider.Provider) (provider.Identity, error) {
	if _, err := apiVersion(p); err != nil {
		return provider.Identity{}, err
	}
	if _, err := a.listDeployments(ctx, c, p); err != nil {
		return provider.Identity{}, err
	}
	id := provider.Identity{KeyLabel: c.Label, KeyScope: "resource"}
	if d := deployment(p); d != "" {
		id.Account = d
	}
	return id, nil
}

// ListModels returns deployments, which are the routable model names in Azure.
func (a *Adapter) ListModels(ctx context.Context, c provider.Credential, p provider.Provider) ([]provider.ModelInfo, error) {
	return a.listDeployments(ctx, c, p)
}

// Usage is unsupported: Azure OpenAI exposes no usage history to a data-plane key.
func (a *Adapter) Usage(context.Context, provider.Credential, provider.Provider, provider.Window) ([]provider.UsageBucket, error) {
	return nil, fmt.Errorf("%w: Azure OpenAI usage history needs Azure Monitor", provider.ErrUnsupported)
}

// Costs is unsupported: spend lives in Azure Cost Management.
func (a *Adapter) Costs(context.Context, provider.Credential, provider.Provider, provider.Window) ([]provider.CostBucket, error) {
	return nil, fmt.Errorf("%w: Azure OpenAI costs need Azure Cost Management", provider.ErrUnsupported)
}

// Balance is unsupported: Azure is billed to a subscription.
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
