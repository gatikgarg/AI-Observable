package openai

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ai-observable/ai-observable/internal/provider"
)

type usageResponse struct {
	Data []struct {
		StartTime int64 `json:"start_time"`
		EndTime   int64 `json:"end_time"`
		Results   []struct {
			InputTokens       int64  `json:"input_tokens"`
			OutputTokens      int64  `json:"output_tokens"`
			InputCachedTokens int64  `json:"input_cached_tokens"`
			NumModelRequests  int64  `json:"num_model_requests"`
			Model             string `json:"model"`
			ProjectID         string `json:"project_id"`
			APIKeyID          string `json:"api_key_id"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool   `json:"has_more"`
	NextPage string `json:"next_page"`
}

// Usage returns hourly token and request aggregates.
func (a *Adapter) Usage(ctx context.Context, c provider.Credential, p provider.Provider, w provider.Window) ([]provider.UsageBucket, error) {
	q := url.Values{}
	q.Set("start_time", strconv.FormatInt(w.Start.Unix(), 10))
	q.Set("end_time", strconv.FormatInt(w.End.Unix(), 10))
	q.Set("bucket_width", "1h")
	q.Set("limit", "168")
	q.Add("group_by", "model")
	q.Add("group_by", "project_id")
	q.Add("group_by", "api_key_id")
	endpoint := provider.JoinURL(p.BaseURL, "/v1/organization/usage/completions")
	var out []provider.UsageBucket
	page := ""
	for i := 0; i < maxPages; i++ {
		pq := cloneValues(q)
		if page != "" {
			pq.Set("page", page)
		}
		var resp usageResponse
		if err := provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet, provider.WithQuery(endpoint, pq), nil, &resp); err != nil {
			return nil, err
		}
		for _, b := range resp.Data {
			for _, r := range b.Results {
				out = append(out, provider.UsageBucket{
					Model:     r.Model,
					Start:     unix(b.StartTime),
					End:       unix(b.EndTime),
					Requests:  r.NumModelRequests,
					Input:     r.InputTokens,
					Output:    r.OutputTokens,
					CacheRead: r.InputCachedTokens,
					Project:   r.ProjectID,
					APIKeyID:  r.APIKeyID,
				})
			}
		}
		if !resp.HasMore || resp.NextPage == "" {
			break
		}
		page = resp.NextPage
	}
	return out, nil
}
