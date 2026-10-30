package store

import (
	"context"
	"fmt"
	"time"
)

// InsertUsage upserts provider-reported buckets. Buckets whose values are
// unchanged are left alone, so importing the same fixture twice inserts 0 rows.
func (s *Store) InsertUsage(ctx context.Context, rows []UsageRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	st, err := tx.PrepareContext(ctx, `INSERT INTO provider_usage(
		provider,model,project,api_key_id,bucket_start_ms,bucket_end_ms,granularity,
		requests,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,
		cost_usd,currency,source,raw_hash,imported_at_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(provider,model,project,bucket_start_ms,bucket_end_ms,source) DO UPDATE SET
			granularity=excluded.granularity, api_key_id=excluded.api_key_id,
			requests=excluded.requests, input_tokens=excluded.input_tokens,
			output_tokens=excluded.output_tokens, cache_read_tokens=excluded.cache_read_tokens,
			cache_write_tokens=excluded.cache_write_tokens, cost_usd=excluded.cost_usd,
			currency=excluded.currency, raw_hash=excluded.raw_hash, imported_at_ms=excluded.imported_at_ms
		WHERE provider_usage.requests IS NOT excluded.requests
		   OR provider_usage.input_tokens IS NOT excluded.input_tokens
		   OR provider_usage.output_tokens IS NOT excluded.output_tokens
		   OR provider_usage.cache_read_tokens IS NOT excluded.cache_read_tokens
		   OR provider_usage.cache_write_tokens IS NOT excluded.cache_write_tokens
		   OR provider_usage.cost_usd IS NOT excluded.cost_usd`)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	now := time.Now().UnixMilli()
	inserted := 0
	for _, r := range rows {
		// Empty model/project/key are stored as '' rather than NULL: SQLite
		// treats NULLs as distinct in UNIQUE, and a re-import of the same not-
		// reported bucket must update in place instead of duplicating.
		res, err := st.ExecContext(ctx, r.Provider, r.Model, r.Project, r.APIKeyID,
			r.BucketStartMS, r.BucketEndMS, r.Granularity, r.Requests, r.Input, r.Output,
			r.CacheRead, r.CacheWrite, r.CostUSD, nullstr(r.Currency), r.Source, nullstr(r.RawHash), now)
		if err != nil {
			return inserted, fmt.Errorf("store: insert usage: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	return inserted, tx.Commit()
}

// InsertCatalog upserts model catalog rows, keeping a previously known price
// when the provider reports none.
func (s *Store) InsertCatalog(ctx context.Context, rows []CatalogRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	st, err := tx.PrepareContext(ctx, `INSERT INTO model_catalog(
		provider,model_id,display_name,context_max,input_price,output_price,
		cache_read_price,cache_write_price,price_source,capabilities,first_seen_ms,last_seen_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(provider,model_id) DO UPDATE SET
			display_name=COALESCE(excluded.display_name,model_catalog.display_name),
			context_max=COALESCE(excluded.context_max,model_catalog.context_max),
			input_price=COALESCE(excluded.input_price,model_catalog.input_price),
			output_price=COALESCE(excluded.output_price,model_catalog.output_price),
			cache_read_price=COALESCE(excluded.cache_read_price,model_catalog.cache_read_price),
			cache_write_price=COALESCE(excluded.cache_write_price,model_catalog.cache_write_price),
			price_source=COALESCE(NULLIF(excluded.price_source,''),model_catalog.price_source),
			capabilities=COALESCE(NULLIF(excluded.capabilities,''),model_catalog.capabilities),
			last_seen_ms=excluded.last_seen_ms`)
	if err != nil {
		return 0, err
	}
	defer st.Close()
	now := time.Now().UnixMilli()
	inserted := 0
	for _, r := range rows {
		first := r.FirstSeenMS
		if first == 0 {
			first = now
		}
		last := r.LastSeenMS
		if last == 0 {
			last = now
		}
		res, err := st.ExecContext(ctx, r.Provider, r.ModelID, nullstr(r.DisplayName), r.ContextMax,
			r.InputPrice, r.OutputPrice, r.CacheReadPrice, r.CacheWritePrice,
			nullstr(r.PriceSource), nullstr(r.Capabilities), first, last)
		if err != nil {
			return inserted, fmt.Errorf("store: insert catalog: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			inserted++
		}
	}
	return inserted, tx.Commit()
}
