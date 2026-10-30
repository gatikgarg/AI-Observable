// Package deepseek implements the DeepSeek provider family: the model catalog
// and prepaid balance. DeepSeek publishes no usage or cost history API.
package deepseek

import (
	"context"
	"net/http"
	"strconv"

	"github.com/ai-observable/ai-observable/internal/provider"
)

// Adapter implements provider.Adapter for DeepSeek.
type Adapter struct{ hc *http.Client }

// New returns an adapter using the shared provider client.
func New() *Adapter { return &Adapter{hc: provider.Client()} }

func init() { provider.Register(New()) }

// Kind returns the preset kind.
func (a *Adapter) Kind() string { return "deepseek" }

// Capabilities reports what DeepSeek exposes to a key.
func (a *Adapter) Capabilities() provider.Capabilities {
	return provider.Capabilities{Models: true, Balance: true}
}

// ParseUsage normalizes a chat/completions payload or SSE stream. DeepSeek
// reports prompt_cache_hit_tokens and prompt_cache_miss_tokens; the generic
// normalizer maps the hit count to CacheRead and the miss count to CacheWrite.
func (a *Adapter) ParseUsage(stream bool, contentType string, body []byte) (provider.Usage, error) {
	return provider.ParseGeneric(stream, contentType, body)
}

type modelsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// Validate proves the key works with the cheapest call available.
func (a *Adapter) Validate(ctx context.Context, c provider.Credential, p provider.Provider) (provider.Identity, error) {
	var out modelsResponse
	if err := provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet,
		provider.JoinURL(p.BaseURL, "/v1/models"), nil, &out); err != nil {
		return provider.Identity{}, err
	}
	return provider.Identity{KeyLabel: c.Label, KeyScope: "account"}, nil
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
		infos = append(infos, provider.ModelInfo{ID: m.ID, DisplayName: m.ID})
	}
	return infos, nil
}

type balanceResponse struct {
	IsAvailable  bool `json:"is_available"`
	BalanceInfos []struct {
		Currency        string `json:"currency"`
		TotalBalance    string `json:"total_balance"`
		GrantedBalance  string `json:"granted_balance"`
		ToppedUpBalance string `json:"topped_up_balance"`
	} `json:"balance_infos"`
}

// Balance returns the prepaid credit balance.
func (a *Adapter) Balance(ctx context.Context, c provider.Credential, p provider.Provider) (*provider.Balance, error) {
	var out balanceResponse
	if err := provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet,
		provider.JoinURL(p.BaseURL, "/user/balance"), nil, &out); err != nil {
		return nil, err
	}
	if len(out.BalanceInfos) == 0 {
		return nil, provider.ErrUnsupported
	}
	info := out.BalanceInfos[0]
	amount, err := strconv.ParseFloat(info.TotalBalance, 64)
	if err != nil {
		return nil, provider.ErrUnsupported
	}
	return &provider.Balance{Amount: amount, Currency: info.Currency}, nil
}

// Usage is unsupported: DeepSeek has no usage history endpoint.
func (a *Adapter) Usage(context.Context, provider.Credential, provider.Provider, provider.Window) ([]provider.UsageBucket, error) {
	return nil, provider.ErrUnsupported
}

// Costs is unsupported: DeepSeek has no cost history endpoint.
func (a *Adapter) Costs(context.Context, provider.Credential, provider.Provider, provider.Window) ([]provider.CostBucket, error) {
	return nil, provider.ErrUnsupported
}
