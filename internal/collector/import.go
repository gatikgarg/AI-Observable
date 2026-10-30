package collector

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ai-observable/ai-observable/internal/pricing"
	"github.com/ai-observable/ai-observable/internal/provider"
	"github.com/ai-observable/ai-observable/internal/store"
)

// importUsage writes provider-reported usage buckets. The natural key
// (provider, model, project, key, bucket) plus a stable payload hash make a
// re-import update in place instead of duplicating rows.
func (c *Collector) importUsage(ctx context.Context, a provider.Adapter, cred provider.Credential,
	prov provider.Provider, name string, w provider.Window) (int, int64, error) {
	buckets, err := a.Usage(ctx, cred, prov, w)
	if err != nil {
		return 0, 0, err
	}
	rows := make([]store.UsageRow, 0, len(buckets))
	var requests int64
	for _, b := range buckets {
		if b.End.IsZero() || !b.End.After(b.Start) {
			continue
		}
		req, in, out := b.Requests, b.Input, b.Output
		cr, cw := b.CacheRead, b.CacheWrite
		rows = append(rows, store.UsageRow{
			Provider: name, Model: b.Model, Project: b.Project, APIKeyID: b.APIKeyID,
			BucketStartMS: b.Start.UnixMilli(), BucketEndMS: b.End.UnixMilli(),
			Granularity: grain(b.End.Sub(b.Start)),
			Requests:    &req, Input: &in, Output: &out, CacheRead: &cr, CacheWrite: &cw,
			CostUSD: b.CostUSD, Currency: "USD", Source: "usage_api",
			RawHash: rowHash(name, b.Model, b.Project, b.APIKeyID, b.Start.UnixMilli(), b.Requests, b.Input, b.Output),
		})
		requests += b.Requests
	}
	n, err := c.Store.InsertUsage(ctx, rows)
	return n, requests, err
}

// importCosts writes provider-reported spend. Costs carry no request counts, so
// they can never inflate the calls side of a reconciliation.
func (c *Collector) importCosts(ctx context.Context, a provider.Adapter, cred provider.Credential,
	prov provider.Provider, name string, w provider.Window) (int, int64, error) {
	buckets, err := a.Costs(ctx, cred, prov, w)
	if err != nil {
		return 0, 0, err
	}
	rows := make([]store.UsageRow, 0, len(buckets))
	for _, b := range buckets {
		if b.End.IsZero() || !b.End.After(b.Start) {
			continue
		}
		amount := b.Amount
		cur := b.Currency
		if cur == "" {
			cur = "USD"
		}
		rows = append(rows, store.UsageRow{
			Provider: name, Model: b.LineItem,
			BucketStartMS: b.Start.UnixMilli(), BucketEndMS: b.End.UnixMilli(),
			Granularity: grain(b.End.Sub(b.Start)),
			CostUSD:     &amount, Currency: cur, Source: "cost_api",
			RawHash: rowHash(name, b.LineItem, b.Start.UnixMilli(), b.Amount, cur),
		})
	}
	n, err := c.Store.InsertUsage(ctx, rows)
	return n, 0, err
}

// importModels refreshes the catalog and attaches local prices. Prices are only
// ever read from the price module (user file → embedded → compiled-in).
func (c *Collector) importModels(ctx context.Context, a provider.Adapter, cred provider.Credential,
	prov provider.Provider, name string) (int, error) {
	models, err := a.ListModels(ctx, cred, prov)
	if err != nil {
		return 0, err
	}
	now := c.now().UnixMilli()
	rows := make([]store.CatalogRow, 0, len(models))
	for _, m := range models {
		if m.ID == "" {
			continue
		}
		row := store.CatalogRow{
			Provider: name, ModelID: m.ID, DisplayName: m.DisplayName,
			Capabilities: capabilities(m.Capabilities), LastSeenMS: now,
		}
		if m.ContextMax > 0 {
			n := m.ContextMax
			row.ContextMax = &n
		}
		if pr, ok := pricing.Lookup(m.ID); ok {
			in, out, cr, cw := pr.Input, pr.Output, pr.CacheRead, pr.CacheWrite
			row.InputPrice, row.OutputPrice = &in, &out
			row.CacheReadPrice, row.CacheWritePrice = &cr, &cw
			row.PriceSource = "price_file"
			if row.ContextMax == nil && pr.Context > 0 {
				n := pr.Context
				row.ContextMax = &n
			}
		}
		rows = append(rows, row)
	}
	return c.Store.InsertCatalog(ctx, rows)
}

// importBalance records prepaid credit in the meta table: v1 has no balance
// column in the frozen calls view, and the UI only needs the latest reading.
func (c *Collector) importBalance(ctx context.Context, a provider.Adapter, cred provider.Credential,
	prov provider.Provider, name string) (int, error) {
	bal, err := a.Balance(ctx, cred, prov)
	if err != nil {
		return 0, err
	}
	if bal == nil {
		return 0, nil
	}
	cur := bal.Currency
	if cur == "" {
		cur = "USD"
	}
	key := "balance:" + name
	val := fmt.Sprintf(`{"amount":%g,"currency":%q,"at_ms":%d}`,
		bal.Amount, cur, c.now().UnixMilli())
	if err := c.Store.SetMeta(ctx, key, val); err != nil {
		return 0, err
	}
	return 1, nil
}

func capabilities(m map[string]bool) string {
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}
