package collector

import (
	"context"

	"github.com/ai-observable/ai-observable/internal/control"
	"github.com/ai-observable/ai-observable/internal/provider"
	"github.com/ai-observable/ai-observable/internal/store"
)

// reconcile runs the gap_fill residual pass for one usage window. Only
// providers whose row says gap_fill are affected; mirror/off write nothing.
// The import already replaced this window's provider buckets, so the window is
// always the one the run just covered.
func (c *Collector) reconcile(ctx context.Context, p control.Provider, w provider.Window) (int, error) {
	if p.ReconcileMode != "gap_fill" {
		return 0, nil
	}
	start, end := w.Start.UnixMilli(), w.End.UnixMilli()
	if end <= start {
		return 0, nil
	}
	written, cleared, err := c.Store.ReconcileResiduals(ctx, store.Window{
		StartMS: start, EndMS: end, Granularity: "hour",
	})
	if err != nil {
		return written, err
	}
	if cleared > 0 {
		_ = c.DB.Audit(ctx, "collector", "reconcile.clear", p.Name,
			map[string]any{"cleared": cleared, "window_start_ms": start})
	}
	return written, nil
}

// wireReconcileMode lets store ask the control DB for a provider's mode without
// importing control. Safe to call more than once.
func wireReconcileMode(db *control.DB, st *store.Store) {
	if st == nil || st.ReconcileMode != nil {
		return
	}
	st.ReconcileMode = func(ctx context.Context, name string) (string, error) {
		p, err := db.GetProvider(ctx, name)
		if err != nil {
			return "off", err
		}
		return p.ReconcileMode, nil
	}
}
