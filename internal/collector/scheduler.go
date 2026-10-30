package collector

import (
	"context"
	"errors"
	"time"

	"github.com/ai-observable/ai-observable/internal/control"
	"github.com/ai-observable/ai-observable/internal/provider"
	"github.com/ai-observable/ai-observable/internal/store"
)

// run executes one lane: resolve the provider, import, and persist the result.
// It always leaves collector_jobs with a status.
func (c *Collector) run(ctx context.Context, j control.Job, full bool) Result {
	started := c.now()
	r := Result{Kind: j.Kind}
	p, err := c.DB.GetProvider(ctx, j.ProviderID)
	if err != nil {
		r.Status, r.Err = "error", errText(err)
		return r // the job row is gone with its provider; nothing to update
	}
	r.Provider = p.Name
	next := started.Add(time.Duration(j.IntervalSeconds) * time.Second)
	if !p.Enabled {
		r.Status, r.Err = "skipped", "provider is disabled"
		return c.finish(ctx, j, p, r, provider.Window{}, nil, next.UnixMilli())
	}
	a, supported, err := adapterFor(p, j.Kind)
	if err != nil {
		r.Status, r.Err = "error", errText(err)
		return c.finish(ctx, j, p, r, provider.Window{}, nil, next.UnixMilli())
	}
	if !supported {
		r.Status, r.Err = "unsupported", "provider does not expose "+j.Kind
		return c.finish(ctx, j, p, r, provider.Window{}, nil, next.UnixMilli())
	}
	// Pull lanes need a key that lives on this machine. auth_mode=client never
	// has one, so the lane is visibly unsupported instead of silently empty.
	secret, credID, err := c.DB.ActiveSecretFor(ctx, p.ID, c.Vault)
	if err != nil {
		r.Status, r.Err = "error", "no usable credential: "+errText(err)
		return c.finish(ctx, j, p, r, provider.Window{}, nil, next.UnixMilli())
	}
	cred := provider.Credential{Secret: secret, ID: credID}
	prov := p.Seam()
	w := c.window(j, full, started)

	runCtx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()
	var rows int
	var reqs int64
	err = c.retry(runCtx, func() error {
		var lerr error
		switch j.Kind {
		case "models":
			rows, lerr = c.importModels(runCtx, a, cred, prov, p.Name)
		case "usage":
			rows, reqs, lerr = c.importUsage(runCtx, a, cred, prov, p.Name, w)
		case "costs":
			rows, reqs, lerr = c.importCosts(runCtx, a, cred, prov, p.Name, w)
		case "balance":
			rows, lerr = c.importBalance(runCtx, a, cred, prov, p.Name)
		}
		return lerr
	})
	if err != nil {
		r.Status, r.Err = laneStatus(err), errText(err)
		if st := provider.StatusOf(err); st == 401 || st == 403 {
			_ = c.DB.SetCredentialStatus(ctx, credID, credentialStatus(st), r.Err)
		}
		return c.finish(ctx, j, p, r, w, nil,
			c.backoffNext(started, j.IntervalSeconds, j.ConsecutiveFailures+1).UnixMilli())
	}
	if terr := c.DB.TouchCredential(ctx, credID); terr != nil {
		r.Status, r.Err = "partial", "credential touch: "+errText(terr)
	}
	r.Status, r.Rows, r.Requests = "ok", rows, reqs
	if r.Err == "" && j.Kind == "usage" {
		if n, rerr := c.reconcile(ctx, p, w); rerr != nil {
			r.Status, r.Err = "partial", "reconcile: "+errText(rerr)
		} else if n > 0 {
			_ = c.DB.Audit(ctx, "collector", "reconcile.gap_fill", p.Name,
				map[string]any{"written": n, "window_start_ms": w.Start.UnixMilli()})
		}
	}
	cursor := w.End.UnixMilli()
	return c.finish(ctx, j, p, r, w, &cursor, next.UnixMilli())
}

// finish writes the import ledger, the job row and the audit entry.
func (c *Collector) finish(ctx context.Context, j control.Job, p control.Provider,
	r Result, w provider.Window, cursor *int64, next int64) Result {
	fin := c.now().UnixMilli()
	start := w.Start.UnixMilli()
	end := w.End.UnixMilli()
	if end == 0 {
		end = fin
	}
	if start == 0 || start > end {
		start = end
	}
	if _, err := c.Store.RecordImport(ctx, store.ImportRow{
		Provider: p.Name, Kind: j.Kind, WindowStartMS: start, WindowEndMS: end,
		Status: jStatus(r.Status), RowsImported: int64(r.Rows), RequestsImported: r.Requests,
		ErrText: r.Err, StartedAtMS: fin, FinishedAtMS: &fin,
	}); err != nil && r.Err == "" {
		r.Status, r.Err = "partial", "import ledger: "+errText(err)
	}
	if err := c.DB.UpdateJobRun(ctx, j.ID, jStatus(r.Status), r.Err, cursor, next); err != nil && r.Err == "" {
		r.Status, r.Err = "partial", "job update: "+errText(err)
	}
	_ = c.DB.Audit(ctx, "collector", "collect."+j.Kind, p.Name, map[string]any{
		"status": r.Status, "rows": r.Rows, "requests": r.Requests, "error": r.Err,
		"window_start_ms": start, "window_end_ms": end,
	})
	if r.Rows > 0 && r.Status != "error" {
		c.requestSnapshot()
	}
	return r
}

// window is the half-open interval one run covers. Cursors make re-runs
// idempotent; a first run backfills collector.backfill_days.
func (c *Collector) window(j control.Job, full bool, now time.Time) provider.Window {
	gran := time.Hour
	end := now.Truncate(gran)
	if j.Kind == "costs" {
		gran = 24 * time.Hour
		end = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	}
	days := c.Config.Collector.BackfillDays
	if days <= 0 {
		days = 7
	}
	if days > 31 && j.Kind == "costs" {
		days = 31 // provider cost APIs cap the lookback
	}
	start := end.AddDate(0, 0, -days)
	if j.CursorMS != nil && !full {
		if s := time.UnixMilli(*j.CursorMS).UTC(); s.After(start) {
			start = s
		}
	}
	if !start.Before(end) {
		start = end.Add(-time.Duration(j.IntervalSeconds) * time.Second)
	}
	return provider.Window{Start: start, End: end}
}

// backoffNext is min(1h, 30s*2^failures) with +jitter, never shorter than the
// normal interval (a failing lane must not poll faster than a healthy one).
func (c *Collector) backoffNext(now time.Time, intervalSeconds, failures int) time.Time {
	if failures < 1 {
		failures = 1
	}
	if failures > 11 {
		failures = 11
	}
	d := 30 * time.Second * time.Duration(1<<uint(failures-1))
	if d > time.Hour {
		d = time.Hour
	}
	if floor := time.Duration(intervalSeconds) * time.Second; d < floor {
		d = floor
	}
	return now.Add(d + jitter(d/5))
}

// adapterFor resolves the adapter and reports whether it exposes the lane.
func adapterFor(p control.Provider, kind string) (provider.Adapter, bool, error) {
	a, err := provider.For(p.Kind)
	if err != nil {
		return nil, false, err
	}
	return a, a.Capabilities().Has(kind), nil
}

// laneStatus maps an adapter error onto a ledger status.
func laneStatus(err error) string {
	if errors.Is(err, provider.ErrUnsupported) {
		return "unsupported"
	}
	return "error"
}

func credentialStatus(httpStatus int) string {
	if httpStatus == 403 {
		return "insufficient_scope"
	}
	return "invalid"
}

// jStatus keeps the ledger vocabulary in sync with collector_jobs.
func jStatus(status string) string {
	switch status {
	case "ok", "partial", "unsupported", "error", "skipped":
		return status
	}
	return "error"
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}
