package store

import (
	"context"
	"fmt"
	"strings"
)

// syntheticTracePrefix marks every row gap_fill ever wrote, so it can be
// cleared again without guessing.
const syntheticTracePrefix = "reconcile:"

// ClearSynthetic removes the gap-fill rows for one provider (or all providers
// when provider is empty) and reports how many rows it deleted.
func (s *Store) ClearSynthetic(ctx context.Context, provider string) (int, error) {
	q := `DELETE FROM calls WHERE trace_id LIKE ?`
	args := []any{syntheticTracePrefix + "%"}
	if provider != "" {
		q += ` AND provider=?`
		args = append(args, provider)
	}
	res, err := s.DB.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("store: clear synthetic: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ReconcileResiduals rewrites the gap-fill rows for w. It only ever writes the
// residual (provider-reported requests minus proxy-seen calls), one row per
// missing call, so sum(calls in bucket) never exceeds provider_usage.requests.
// Providers whose reconcile_mode is not "gap_fill" are left untouched.
func (s *Store) ReconcileResiduals(ctx context.Context, w Window) (written, cleared int, err error) {
	if w.EndMS <= w.StartMS {
		return 0, 0, fmt.Errorf("store: reconcile window %d..%d is empty", w.StartMS, w.EndMS)
	}
	if w.Granularity == "" {
		w.Granularity = "hour"
	}
	if w.Granularity != "hour" && w.Granularity != "day" {
		return 0, 0, fmt.Errorf("store: reconcile granularity %q must be hour or day", w.Granularity)
	}
	res, err := s.DB.ExecContext(ctx, `DELETE FROM calls
		WHERE trace_id LIKE ? AND ts_ms>=? AND ts_ms<?`,
		syntheticTracePrefix+"%", w.StartMS, w.EndMS)
	if err != nil {
		return 0, 0, fmt.Errorf("store: reconcile clear: %w", err)
	}
	n, _ := res.RowsAffected()
	cleared = int(n)
	if s.ReconcileMode == nil {
		return 0, cleared, nil
	}

	rows, err := s.DB.QueryContext(ctx, `SELECT provider,COALESCE(model,''),bucket_start_ms,bucket_end_ms,
		COALESCE(SUM(requests),0),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),
		SUM(cost_usd)
		FROM provider_usage WHERE bucket_start_ms>=? AND bucket_end_ms<=?
		GROUP BY provider,COALESCE(model,''),bucket_start_ms,bucket_end_ms
		ORDER BY provider,bucket_start_ms,model`, w.StartMS, w.EndMS)
	if err != nil {
		return 0, cleared, fmt.Errorf("store: reconcile buckets: %w", err)
	}
	type bucket struct {
		provider      string
		model         string
		start, end    int64
		requests      int64
		input, output int64
		cost          *float64
	}
	buckets := []bucket{}
	for rows.Next() {
		var b bucket
		if err = rows.Scan(&b.provider, &b.model, &b.start, &b.end, &b.requests, &b.input, &b.output, &b.cost); err != nil {
			rows.Close()
			return 0, cleared, err
		}
		buckets = append(buckets, b)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, cleared, err
	}

	modes := map[string]string{}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, cleared, err
	}
	defer tx.Rollback()
	st, err := tx.PrepareContext(ctx, `INSERT INTO calls(
		ts_ms,source,provider,endpoint,model_requested,model_served,stream,status,success,
		latency_ms,input_tokens,output_tokens,usage_estimated,cost_usd,pricing_missing,trace_id,attempt)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, cleared, err
	}
	defer st.Close()
	for _, b := range buckets {
		mode, ok := modes[b.provider]
		if !ok {
			mode, err = s.ReconcileMode(ctx, b.provider)
			if err != nil {
				return written, cleared, fmt.Errorf("store: reconcile mode %s: %w", b.provider, err)
			}
			modes[b.provider] = mode
		}
		if mode != "gap_fill" {
			continue
		}
		var seen int64
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM calls
			WHERE provider=? AND source='proxy' AND ts_ms>=? AND ts_ms<?
			  AND COALESCE(NULLIF(model_served,''),model_requested,'unknown')=?`,
			b.provider, b.start, b.end, b.model).Scan(&seen); err != nil {
			return written, cleared, err
		}
		residual := b.requests - seen
		if residual <= 0 {
			continue
		}
		in, out := splitEven(b.input, residual), splitEven(b.output, residual)
		unitCost := 0.0
		if b.cost != nil {
			unitCost = *b.cost / float64(residual)
		}
		model := b.model
		if model == "" {
			model = "unknown"
		}
		for i := int64(0); i < residual; i++ {
			rowIn, rowOut := in, out
			var rowCost *float64
			if b.cost != nil {
				v := unitCost
				if i == 0 { // keep the aggregate exact: the remainder lands on row 0
					rowIn = b.input - in*(residual-1)
					rowOut = b.output - out*(residual-1)
					v = *b.cost - unitCost*float64(residual-1)
				}
				rowCost = &v
			}
			if _, err = st.ExecContext(ctx, b.start+i, "usage_api", b.provider, "usage_api/aggregate",
				model, model, 0, 200, 1, 0, rowIn, rowOut, 1, rowCost, 0,
				syntheticTracePrefix+fmt.Sprint(b.start), 1); err != nil {
				return written, cleared, fmt.Errorf("store: reconcile insert: %w", err)
			}
			written++
		}
	}
	return written, cleared, tx.Commit()
}

func splitEven(total, parts int64) int64 {
	if parts <= 0 || total <= 0 {
		return 0
	}
	return total / parts
}

// SyntheticTraceID is the trace_id assigned to every gap-fill row for a bucket.
func SyntheticTraceID(bucketStartMS int64) string {
	return syntheticTracePrefix + fmt.Sprint(bucketStartMS)
}

// IsSyntheticTrace reports whether a trace_id was written by gap_fill.
func IsSyntheticTrace(traceID string) bool {
	return strings.HasPrefix(traceID, syntheticTracePrefix)
}
