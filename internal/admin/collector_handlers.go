package admin

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ai-observable/ai-observable/internal/control"
)

// jobView adds the provider name so the Collection tab does not have to look it
// up in the browser.
type jobView struct {
	control.Job
	Provider string `json:"provider"`
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	db := s.controlReady(w)
	if db == nil {
		return
	}
	ctx := r.Context()
	ref := queryString(r, "provider_id")
	if ref == "" {
		ref = queryString(r, "provider")
	}
	rows, err := db.ListJobs(ctx, ref)
	if err != nil {
		fail(w, err)
		return
	}
	names := map[string]string{}
	if ref == "" {
		providers, perr := db.ListProviders(ctx)
		if perr != nil {
			fail(w, perr)
			return
		}
		for _, p := range providers {
			names[p.ID] = p.Name
		}
	} else if p, perr := db.GetProvider(ctx, ref); perr == nil {
		names[p.ID] = p.Name
	}
	out := make([]jobView, 0, len(rows))
	for _, j := range rows {
		out = append(out, jobView{Job: j, Provider: names[j.ProviderID]})
	}
	ok(w, out)
}

// patchJob is how the UI pauses and resumes a lane. The cursor is untouched:
// only a run advances it.
func (s *Server) patchJob(w http.ResponseWriter, r *http.Request) {
	db := s.controlReady(w)
	if db == nil {
		return
	}
	var body struct {
		Enabled         *bool `json:"enabled"`
		IntervalSeconds *int  `json:"interval_seconds"`
	}
	if !decodeOptional(w, r, &body) {
		return
	}
	ctx := s.uiCtx(r)
	job, err := db.GetJob(ctx, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if body.Enabled == nil && body.IntervalSeconds == nil {
		ok(w, jobView{Job: job, Provider: s.providerName(ctx, db, job.ProviderID)})
		return
	}
	if body.Enabled != nil {
		job.Enabled = *body.Enabled
	}
	if body.IntervalSeconds != nil {
		if *body.IntervalSeconds < 30 {
			writeErr(w, http.StatusBadRequest, "bad_request", "interval_seconds must be at least 30")
			return
		}
		job.IntervalSeconds = *body.IntervalSeconds
	}
	updated, err := db.UpsertJob(ctx, job)
	if err != nil {
		fail(w, err)
		return
	}
	name := s.providerName(ctx, db, updated.ProviderID)
	if err = db.Audit(ctx, "ui", "job.update", updated.ID, map[string]any{
		"provider": name, "kind": updated.Kind, "enabled": updated.Enabled,
		"interval_seconds": updated.IntervalSeconds,
	}); err != nil {
		fail(w, err)
		return
	}
	s.Logs.Add("info", "collector lane updated", "provider", name, "kind", updated.Kind,
		"enabled", strconv.FormatBool(updated.Enabled), "every", strconv.Itoa(updated.IntervalSeconds)+"s")
	ok(w, jobView{Job: updated, Provider: name})
}

func (s *Server) providerName(ctx context.Context, db *control.DB, id string) string {
	if p, err := db.GetProvider(ctx, id); err == nil {
		return p.Name
	}
	return id
}

// runCollector queues lanes in the background and answers 202. The UI polls the
// import ledger, so nothing here waits for a provider API.
func (s *Server) runCollector(w http.ResponseWriter, r *http.Request) {
	if s.Collector == nil {
		writeErr(w, http.StatusServiceUnavailable, "unsupported",
			"the collector is disabled; set [collector] enabled = true in config.toml")
		return
	}
	db := s.controlReady(w)
	if db == nil {
		return
	}
	var body struct {
		ProviderID   string `json:"provider_id"`
		Kind         string `json:"kind"`
		FullBackfill bool   `json:"full_backfill"`
	}
	if !decodeOptional(w, r, &body) {
		return
	}
	ref := strings.TrimSpace(body.ProviderID)
	if ref == "" {
		ref = queryString(r, "provider_id")
	}
	kind := strings.TrimSpace(body.Kind)
	if kind != "" {
		valid := false
		for _, k := range control.JobKinds {
			if k == kind {
				valid = true
			}
		}
		if !valid {
			writeErr(w, http.StatusBadRequest, "bad_request",
				"kind must be one of "+strings.Join(control.JobKinds, ", "))
			return
		}
	}
	name := ""
	if ref != "" {
		p, err := db.GetProvider(r.Context(), ref)
		if err != nil {
			fail(w, err)
			return
		}
		ref, name = p.ID, p.Name
	}
	if !s.queueCollect(r, ref, kind, body.FullBackfill) {
		writeErr(w, http.StatusServiceUnavailable, "unsupported", "the collector is not running")
		return
	}
	s.Logs.Add("info", "collector run queued", "provider", name, "kind", kind,
		"full_backfill", strconv.FormatBool(body.FullBackfill))
	accepted(w, map[string]any{"accepted": true, "provider_id": ref, "provider": name,
		"kind": kind, "full_backfill": body.FullBackfill})
}

// queueCollect runs lanes off the request goroutine and reports whether a
// collector is configured. WithoutCancel keeps the values of the request
// context (actor, trace) while dropping its deadline, which ends with the
// response.
func (s *Server) queueCollect(r *http.Request, providerRef, kind string, full bool) bool {
	if s.Collector == nil {
		return false
	}
	base := context.WithoutCancel(r.Context())
	go func() {
		ctx, cancel := context.WithTimeout(control.WithActor(base, "collector"), 5*time.Minute)
		defer cancel()
		start := time.Now()
		res, err := s.Collector.RunNow(ctx, providerRef, kind, full)
		for _, x := range res {
			level := "info"
			if x.Status != "ok" {
				level = "warn"
			}
			s.Logs.Add(level, "collector "+x.Kind, "provider", x.Provider, "status", x.Status,
				"rows", strconv.Itoa(x.Rows), "requests", strconv.FormatInt(x.Requests, 10),
				"error", x.Err)
		}
		if err != nil {
			s.Logs.Add("error", "collector run failed", "provider", providerRef, "error", err.Error())
		} else if len(res) == 0 {
			s.Logs.Add("info", "collector run found no lanes", "provider", providerRef, "kind", kind)
		}
		s.Logs.Add("debug", "collector run finished", "ms",
			strconv.FormatInt(time.Since(start).Milliseconds(), 10))
	}()
	return true
}

// importView is one row of the import ledger in the UI's vocabulary.
type importView struct {
	Provider         string   `json:"provider"`
	Kind             string   `json:"kind"`
	WindowStartMS    int64    `json:"window_start_ms"`
	WindowEndMS      int64    `json:"window_end_ms"`
	Status           string   `json:"status"`
	RowsImported     int64    `json:"rows_imported"`
	RequestsImported int64    `json:"requests_imported"`
	CostImportedUSD  *float64 `json:"cost_imported_usd,omitempty"`
	Error            string   `json:"error,omitempty"`
	StartedAtMS      int64    `json:"started_at_ms"`
	FinishedAtMS     *int64   `json:"finished_at_ms,omitempty"`
}

// collectorRuns reports every import attempt, newest first. Rows are the
// collector's own ledger, not a dashboard aggregate.
func (s *Server) collectorRuns(w http.ResponseWriter, r *http.Request) {
	st := s.storeReady(w)
	if st == nil {
		return
	}
	rows, err := st.ImportRows(r.Context(), queryLimit(r, 50, 500))
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]importView, 0, len(rows))
	for _, x := range rows {
		out = append(out, importView{
			Provider: x.Provider, Kind: x.Kind,
			WindowStartMS: x.WindowStartMS, WindowEndMS: x.WindowEndMS, Status: x.Status,
			RowsImported: x.RowsImported, RequestsImported: x.RequestsImported,
			CostImportedUSD: x.CostImportedUSD, Error: x.ErrText,
			StartedAtMS: x.StartedAtMS, FinishedAtMS: x.FinishedAtMS,
		})
	}
	ok(w, out)
}
