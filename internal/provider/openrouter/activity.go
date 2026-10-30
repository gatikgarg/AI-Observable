package openrouter

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/ai-observable/ai-observable/internal/provider"
)

type activityResponse struct {
	Data []struct {
		Date             string  `json:"date"`
		Model            string  `json:"model"`
		Provider         string  `json:"provider_name"`
		Requests         int64   `json:"requests"`
		PromptTokens     int64   `json:"prompt_tokens"`
		CompletionTokens int64   `json:"completion_tokens"`
		ReasoningTokens  int64   `json:"reasoning_tokens"`
		Usage            float64 `json:"usage"`
	} `json:"data"`
}

// activityDays lists the UTC days touched by a window, capped so a wide window
// cannot turn into hundreds of API calls.
func activityDays(w provider.Window) []time.Time {
	const maxDays = 31
	s := w.Start.UTC()
	start := time.Date(s.Year(), s.Month(), s.Day(), 0, 0, 0, 0, time.UTC)
	end := w.End.UTC()
	var out []time.Time
	for d := start; d.Before(end) && len(out) < maxDays; d = d.AddDate(0, 0, 1) {
		out = append(out, d)
	}
	return out
}

func (a *Adapter) activity(ctx context.Context, c provider.Credential, p provider.Provider, day time.Time) (activityResponse, error) {
	var out activityResponse
	q := url.Values{"date": {day.Format("2006-01-02")}}
	err := provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet,
		provider.WithQuery(provider.JoinURL(p.BaseURL, "/v1/activity"), q), nil, &out)
	return out, err
}

// Usage returns daily per-model token aggregates.
func (a *Adapter) Usage(ctx context.Context, c provider.Credential, p provider.Provider, w provider.Window) ([]provider.UsageBucket, error) {
	var out []provider.UsageBucket
	for _, day := range activityDays(w) {
		resp, err := a.activity(ctx, c, p, day)
		if err != nil {
			return nil, err
		}
		for _, r := range resp.Data {
			out = append(out, provider.UsageBucket{
				Model:    r.Model,
				Start:    day,
				End:      day.Add(24 * time.Hour),
				Requests: r.Requests,
				Input:    r.PromptTokens,
				Output:   r.CompletionTokens,
				Project:  r.Provider,
			})
		}
	}
	return out, nil
}

// Costs returns the money OpenRouter reported per day and model.
func (a *Adapter) Costs(ctx context.Context, c provider.Credential, p provider.Provider, w provider.Window) ([]provider.CostBucket, error) {
	var out []provider.CostBucket
	for _, day := range activityDays(w) {
		resp, err := a.activity(ctx, c, p, day)
		if err != nil {
			return nil, err
		}
		for _, r := range resp.Data {
			if r.Usage == 0 {
				continue
			}
			out = append(out, provider.CostBucket{
				Start:    day,
				End:      day.Add(24 * time.Hour),
				Amount:   r.Usage,
				Currency: "USD",
				LineItem: r.Model,
			})
		}
	}
	return out, nil
}

type creditsResponse struct {
	Data struct {
		TotalCredits float64 `json:"total_credits"`
		TotalUsage   float64 `json:"total_usage"`
	} `json:"data"`
}

// Balance returns remaining prepaid credit.
func (a *Adapter) Balance(ctx context.Context, c provider.Credential, p provider.Provider) (*provider.Balance, error) {
	var out creditsResponse
	if err := provider.Do(ctx, a.hc, p, c.Secret, http.MethodGet,
		provider.JoinURL(p.BaseURL, "/v1/credits"), nil, &out); err != nil {
		return nil, err
	}
	remaining := out.Data.TotalCredits - out.Data.TotalUsage
	if remaining < 0 {
		remaining = 0
	}
	return &provider.Balance{Amount: remaining, Currency: "USD"}, nil
}
