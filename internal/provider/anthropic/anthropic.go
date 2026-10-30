// Package anthropic implements the Anthropic provider family: models, the
// organization usage report and the cost report. The last two need an Admin
// API key (sk-ant-admin...).
package anthropic

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ai-observable/ai-observable/internal/provider"
)

// defaultVersion is the Messages API version this build was verified against.
const defaultVersion = "2023-06-01"

// Adapter implements provider.Adapter for Anthropic.
type Adapter struct{ hc *http.Client }

// New returns an adapter using the shared provider client.
func New() *Adapter { return &Adapter{hc: provider.Client()} }

func init() { provider.Register(New()) }

// Kind returns the preset kind.
func (a *Adapter) Kind() string { return "anthropic" }

// Capabilities reports what Anthropic exposes to a key.
func (a *Adapter) Capabilities() provider.Capabilities {
	return provider.Capabilities{Models: true, Usage: true, Costs: true}
}

// ParseUsage normalizes a messages payload or SSE stream.
//
// Anthropic reports cache reads and writes next to input_tokens instead of
// inside it, so both are folded into Input here: every Usage in this codebase
// means "total prompt tokens", matching OpenAI's prompt_tokens. Pricing then
// subtracts CacheRead once, which keeps the arithmetic identical for every
// provider.
func (a *Adapter) ParseUsage(stream bool, contentType string, body []byte) (provider.Usage, error) {
	u, err := provider.ParseGeneric(stream, contentType, body)
	u.Input += u.CacheRead + u.CacheWrite
	return u, err
}

// version resolves the anthropic-version header from the provider extras.
func version(p provider.Provider) string {
	if v := strings.TrimSpace(p.Extra["anthropic_version"]); v != "" {
		return v
	}
	if v := strings.TrimSpace(p.Extra["anthropic-version"]); v != "" {
		return v
	}
	return defaultVersion
}

func (a *Adapter) call(ctx context.Context, c provider.Credential, p provider.Provider, rawURL string, out any) error {
	return provider.DoHeaders(ctx, a.hc, p, c.Secret, http.MethodGet, rawURL,
		map[string]string{"anthropic-version": version(p)}, nil, out)
}

type modelsResponse struct {
	Data []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		CreatedAt   string `json:"created_at"`
	} `json:"data"`
	HasMore bool   `json:"has_more"`
	LastID  string `json:"last_id"`
}

// Validate proves the key works with the cheapest call available.
func (a *Adapter) Validate(ctx context.Context, c provider.Credential, p provider.Provider) (provider.Identity, error) {
	var out modelsResponse
	if err := a.call(ctx, c, p, provider.JoinURL(p.BaseURL, "/v1/models?limit=1"), &out); err != nil {
		return provider.Identity{}, err
	}
	return provider.Identity{KeyLabel: c.Label, KeyScope: "organization"}, nil
}

// ListModels returns the model catalog.
func (a *Adapter) ListModels(ctx context.Context, c provider.Credential, p provider.Provider) ([]provider.ModelInfo, error) {
	var out []provider.ModelInfo
	after := ""
	for i := 0; i < 5; i++ {
		rawURL := provider.JoinURL(p.BaseURL, "/v1/models?limit=1000")
		if after != "" {
			rawURL = provider.WithQuery(rawURL, url.Values{"after_id": {after}})
		}
		var resp modelsResponse
		if err := a.call(ctx, c, p, rawURL, &resp); err != nil {
			return nil, err
		}
		for _, m := range resp.Data {
			out = append(out, provider.ModelInfo{ID: m.ID, DisplayName: firstNonEmpty(m.DisplayName, m.ID)})
		}
		if !resp.HasMore || resp.LastID == "" || resp.LastID == after {
			break
		}
		after = resp.LastID
	}
	return out, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func unixRFC3339(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func atoi(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}
