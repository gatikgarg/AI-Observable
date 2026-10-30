package anthropic

import (
	"context"
	"net/url"
	"time"

	"github.com/ai-observable/ai-observable/internal/provider"
)

// maxPages bounds report pagination.
const maxPages = 10

type usageReport struct {
	Data []struct {
		StartingAt string `json:"starting_at"`
		EndingAt   string `json:"ending_at"`
		Results    []struct {
			UncachedInputTokens      int64  `json:"uncached_input_tokens"`
			CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
			OutputTokens             int64  `json:"output_tokens"`
			Model                    string `json:"model"`
			APIKeyID                 string `json:"api_key_id"`
			WorkspaceID              string `json:"workspace_id"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool    `json:"has_more"`
	NextPage *string `json:"next_page"`
}

// Usage returns hourly token aggregates from the Admin usage report.
func (a *Adapter) Usage(ctx context.Context, c provider.Credential, p provider.Provider, w provider.Window) ([]provider.UsageBucket, error) {
	q := url.Values{}
	q.Set("starting_at", w.Start.UTC().Format(time.RFC3339))
	q.Set("ending_at", w.End.UTC().Format(time.RFC3339))
	q.Set("bucket_width", "1h")
	q.Set("limit", "168")
	q.Add("group_by[]", "model")
	q.Add("group_by[]", "api_key_id")
	endpoint := provider.JoinURL(p.BaseURL, "/v1/organizations/usage_report/messages")
	var out []provider.UsageBucket
	page := ""
	for i := 0; i < maxPages; i++ {
		pq := url.Values{}
		for k, v := range q {
			pq[k] = v
		}
		if page != "" {
			pq.Set("page", page)
		}
		var resp usageReport
		if err := a.call(ctx, c, p, provider.WithQuery(endpoint, pq), &resp); err != nil {
			return nil, err
		}
		for _, b := range resp.Data {
			start, end := unixRFC3339(b.StartingAt), unixRFC3339(b.EndingAt)
			for _, r := range b.Results {
				// Total prompt tokens, matching the proxy's normalization.
				input := r.UncachedInputTokens + r.CacheReadInputTokens + r.CacheCreationInputTokens
				out = append(out, provider.UsageBucket{
					Model:      r.Model,
					Start:      start,
					End:        end,
					Input:      input,
					Output:     r.OutputTokens,
					CacheRead:  r.CacheReadInputTokens,
					CacheWrite: r.CacheCreationInputTokens,
					Project:    r.WorkspaceID,
					APIKeyID:   r.APIKeyID,
				})
			}
		}
		if !resp.HasMore || resp.NextPage == nil || *resp.NextPage == "" || *resp.NextPage == page {
			break
		}
		page = *resp.NextPage
	}
	return out, nil
}

type costReport struct {
	Data []struct {
		StartingAt string `json:"starting_at"`
		EndingAt   string `json:"ending_at"`
		Results    []struct {
			Amount      any    `json:"amount"`
			Currency    string `json:"currency"`
			CostType    string `json:"cost_type"`
			Description string `json:"description"`
			Model       string `json:"model"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool    `json:"has_more"`
	NextPage *string `json:"next_page"`
}

// Costs returns daily money aggregates from the Admin cost report.
func (a *Adapter) Costs(ctx context.Context, c provider.Credential, p provider.Provider, w provider.Window) ([]provider.CostBucket, error) {
	q := url.Values{}
	q.Set("starting_at", w.Start.UTC().Format(time.RFC3339))
	q.Set("ending_at", w.End.UTC().Format(time.RFC3339))
	q.Set("bucket_width", "1d")
	q.Set("limit", "31")
	endpoint := provider.JoinURL(p.BaseURL, "/v1/organizations/cost_report")
	var out []provider.CostBucket
	page := ""
	for i := 0; i < maxPages; i++ {
		pq := url.Values{}
		for k, v := range q {
			pq[k] = v
		}
		if page != "" {
			pq.Set("page", page)
		}
		var resp costReport
		if err := a.call(ctx, c, p, provider.WithQuery(endpoint, pq), &resp); err != nil {
			return nil, err
		}
		for _, b := range resp.Data {
			start, end := unixRFC3339(b.StartingAt), unixRFC3339(b.EndingAt)
			for _, r := range b.Results {
				out = append(out, provider.CostBucket{
					Start:    start,
					End:      end,
					Amount:   amountValue(r.Amount),
					Currency: firstNonEmpty(r.Currency, "USD"),
					LineItem: firstNonEmpty(r.Description, r.CostType, r.Model),
				})
			}
		}
		if !resp.HasMore || resp.NextPage == nil || *resp.NextPage == "" || *resp.NextPage == page {
			break
		}
		page = *resp.NextPage
	}
	return out, nil
}

// amountValue accepts both the string and numeric encodings Anthropic has used
// for report amounts.
func amountValue(v any) float64 {
	switch x := v.(type) {
	case string:
		return atof(x)
	case float64:
		return x
	}
	return 0
}

// Balance is unsupported: Anthropic reports spend, not prepaid credit.
func (a *Adapter) Balance(context.Context, provider.Credential, provider.Provider) (*provider.Balance, error) {
	return nil, provider.ErrUnsupported
}
