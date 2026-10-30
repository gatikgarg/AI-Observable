package openai

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ai-observable/ai-observable/internal/provider"
)

type costsResponse struct {
	Data []struct {
		StartTime int64 `json:"start_time"`
		EndTime   int64 `json:"end_time"`
		Results   []struct {
			Amount struct {
				Value    float64 `json:"value"`
				Currency string  `json:"currency"`
			} `json:"amount"`
			LineItem  string `json:"line_item"`
			ProjectID string `json:"project_id"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool   `json:"has_more"`
	NextPage string `json:"next_page"`
}

// Costs returns daily money aggregates.
func (a *Adapter) Costs(ctx context.Context, c provider.Credential, p provider.Provider, w provider.Window) ([]provider.CostBucket, error) {
	q := url.Values{}
	q.Set("start_time", strconv.FormatInt(w.Start.Unix(), 10))
	q.Set("end_time", strconv.FormatInt(w.End.Unix(), 10))
	q.Set("bucket_width", "1d")
	q.Set("limit", "31")
	q.Add("group_by", "line_item")
	q.Add("group_by", "project_id")
	endpoint := provider.JoinURL(p.BaseURL, "/v1/organization/costs")
	var out []provider.CostBucket
	page := ""
	for i := 0; i < maxPages; i++ {
		pq := cloneValues(q)
		if page != "" {
			pq.Set("page", page)
		}
		var resp costsResponse
		if err := provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet, provider.WithQuery(endpoint, pq), nil, &resp); err != nil {
			return nil, err
		}
		for _, b := range resp.Data {
			for _, r := range b.Results {
				out = append(out, provider.CostBucket{
					Start:    unix(b.StartTime),
					End:      unix(b.EndTime),
					Amount:   r.Amount.Value,
					Currency: r.Amount.Currency,
					LineItem: r.LineItem,
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

// Balance is unsupported: OpenAI has no prepaid credit endpoint.
func (a *Adapter) Balance(context.Context, provider.Credential, provider.Provider) (*provider.Balance, error) {
	return nil, provider.ErrUnsupported
}
