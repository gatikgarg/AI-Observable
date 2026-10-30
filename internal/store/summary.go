package store

import (
	"context"
	"fmt"
	"strings"
)

// SummaryQuery filters the lanes for a dashboard summary.
type SummaryQuery struct {
	From, To        int64
	Provider, Model string
}

// Summary is the honest totals block behind the Metrics tab: measured calls
// from the proxy, reported cost from lane B, and the fill-rate of every
// provenance column.
type Summary struct {
	Calls, Failures, Synthetic                                   int64
	InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens int64
	ReasoningTokens                                              int64
	CostUSD, CostReportedUSD                                     float64
	PricingMissing                                               int64
	FillRate                                                     map[string]float64
}

// Summary aggregates calls in [From,To] plus provider-reported cost from
// provider_usage for the same window.
func (s *Store) Summary(ctx context.Context, q SummaryQuery) (Summary, error) {
	out := Summary{FillRate: map[string]float64{}}
	where, args := filtered(q.From, q.To, q.Provider, q.Model)
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN success=0 AND NOT (COALESCE(error_type,'')='client_cancelled' OR COALESCE(status,-1)=499) THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN trace_id LIKE 'reconcile:%' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cache_read_tokens),0),COALESCE(SUM(cache_write_tokens),0),
		COALESCE(SUM(reasoning_tokens),0),COALESCE(SUM(cost_usd),0),
		COALESCE(SUM(pricing_missing),0)
		FROM calls `+where, args...).
		Scan(&out.Calls, &out.Failures, &out.Synthetic, &out.InputTokens, &out.OutputTokens,
			&out.CacheReadTokens, &out.CacheWriteTokens, &out.ReasoningTokens, &out.CostUSD, &out.PricingMissing)
	if err != nil {
		return out, fmt.Errorf("store: summary: %w", err)
	}
	pq := `SELECT COALESCE(SUM(cost_usd),0) FROM provider_usage WHERE bucket_start_ms>=? AND bucket_end_ms<=?`
	pargs := []any{q.From, q.To}
	if q.Provider != "" {
		pq += ` AND provider=?`
		pargs = append(pargs, q.Provider)
	}
	if err = s.DB.QueryRowContext(ctx, pq, pargs...).Scan(&out.CostReportedUSD); err != nil {
		return out, fmt.Errorf("store: summary cost: %w", err)
	}
	out.FillRate, err = s.FillRates(ctx, q.From, q.To)
	return out, err
}

// filtered builds the WHERE clause shared by summary and fill-rate queries.
func filtered(from, to int64, provider, model string) (string, []any) {
	var b strings.Builder
	b.WriteString("WHERE ts_ms>=? AND ts_ms<=?")
	args := []any{from, to}
	if provider != "" {
		b.WriteString(" AND provider=?")
		args = append(args, provider)
	}
	if model != "" {
		b.WriteString(" AND COALESCE(NULLIF(model_served,''),model_requested,'unknown')=?")
		args = append(args, model)
	}
	return b.String(), args
}

// FillRates reports, per provenance column, what fraction of calls carried a
// value. A metric at 0.00 is visibly not collected instead of silently zero.
func (s *Store) FillRates(ctx context.Context, from, to int64) (map[string]float64, error) {
	out := map[string]float64{}
	var total, model, input, output, read, write, reasoning, cost, ttft, session, project, trace int64
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN COALESCE(model_served,'')<>'' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN input_tokens IS NOT NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN output_tokens IS NOT NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN cache_read_tokens IS NOT NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN cache_write_tokens IS NOT NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN reasoning_tokens IS NOT NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN cost_usd IS NOT NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN ttft_ms IS NOT NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN COALESCE(session_id,'')<>'' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN COALESCE(project,'')<>'' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN COALESCE(trace_id,'')<>'' THEN 1 ELSE 0 END),0)
		FROM calls WHERE ts_ms>=? AND ts_ms<=?`, from, to).
		Scan(&total, &model, &input, &output, &read, &write, &reasoning, &cost, &ttft, &session, &project, &trace)
	if err != nil {
		return out, fmt.Errorf("store: fill rates: %w", err)
	}
	keys := []string{"model_served", "input_tokens", "output_tokens", "cache_read_tokens",
		"cache_write_tokens", "reasoning_tokens", "cost_usd", "ttft_ms", "session_id", "project", "trace_id"}
	if total == 0 {
		for _, k := range keys {
			out[k] = 0
		}
		return out, nil
	}
	for k, n := range map[string]int64{
		"model_served": model, "input_tokens": input, "output_tokens": output,
		"cache_read_tokens": read, "cache_write_tokens": write, "reasoning_tokens": reasoning,
		"cost_usd": cost, "ttft_ms": ttft, "session_id": session, "project": project, "trace_id": trace,
	} {
		out[k] = float64(n) / float64(total)
	}
	return out, nil
}

// Bucket is one row of the time series behind the Metrics and Costs tabs.
type Bucket struct {
	Provider, Model string
	StartMS, EndMS  int64
	Calls, Failures int64
	Input, Output   int64
	CostUSD         float64
}

// UsageBuckets groups calls by hour or day. Granularity must be hour|day.
func (s *Store) UsageBuckets(ctx context.Context, w Window, provider, model string) ([]Bucket, error) {
	size := int64(3600_000)
	if w.Granularity == "day" {
		size = 24 * 3600_000
	} else if w.Granularity != "" && w.Granularity != "hour" {
		return nil, fmt.Errorf("store: granularity %q must be hour or day", w.Granularity)
	}
	where, args := filtered(w.StartMS, w.EndMS, provider, model)
	q := `SELECT provider,
		COALESCE(NULLIF(model_served,''),model_requested,'unknown') AS model,
		(ts_ms/` + fmt.Sprint(size) + `)*` + fmt.Sprint(size) + ` AS bucket,
		COUNT(*),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cost_usd),0),
		COALESCE(SUM(CASE WHEN success=0 AND NOT (COALESCE(error_type,'')='client_cancelled' OR COALESCE(status,-1)=499) THEN 1 ELSE 0 END),0)
		FROM calls ` + where + ` GROUP BY provider, model, bucket ORDER BY bucket, provider, model`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: usage buckets: %w", err)
	}
	defer rows.Close()
	out := []Bucket{}
	for rows.Next() {
		var b Bucket
		if err = rows.Scan(&b.Provider, &b.Model, &b.StartMS, &b.Calls, &b.Input, &b.Output, &b.CostUSD, &b.Failures); err != nil {
			return nil, err
		}
		b.EndMS = b.StartMS + size
		out = append(out, b)
	}
	return out, rows.Err()
}
