package control

import (
	"context"
	"database/sql"

	"github.com/ai-observable/ai-observable/internal/provider"
)

// Job is one collector lane for one provider.
type Job struct {
	ID                  string `json:"id"`
	ProviderID          string `json:"provider_id"`
	Kind                string `json:"kind"`
	Enabled             bool   `json:"enabled"`
	IntervalSeconds     int    `json:"interval_seconds"`
	CursorMS            *int64 `json:"cursor_ms,omitempty"`
	NextRunAtMS         *int64 `json:"next_run_at_ms,omitempty"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	LastRunAtMS         *int64 `json:"last_run_at_ms,omitempty"`
	LastStatus          string `json:"last_status,omitempty"`
	LastError           string `json:"last_error,omitempty"`
}

// JobKinds lists every lane the collector knows.
var JobKinds = []string{"models", "usage", "costs", "balance"}

const jobCols = `id,provider_id,kind,enabled,interval_seconds,cursor_ms,next_run_at_ms,
	consecutive_failures,last_run_at_ms,COALESCE(last_status,''),COALESCE(last_error,'')`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var enabled int
	var cursor, next, last sql.NullInt64
	err := row.Scan(&j.ID, &j.ProviderID, &j.Kind, &enabled, &j.IntervalSeconds, &cursor, &next,
		&j.ConsecutiveFailures, &last, &j.LastStatus, &j.LastError)
	if err != nil {
		return Job{}, err
	}
	j.Enabled = enabled == 1
	if cursor.Valid {
		j.CursorMS = &cursor.Int64
	}
	if next.Valid {
		j.NextRunAtMS = &next.Int64
	}
	if last.Valid {
		j.LastRunAtMS = &last.Int64
	}
	return j, nil
}

// defaultIntervalSeconds keeps provider APIs and our own budget polite. The
// collector honours these unless config.toml overrides them.
func defaultIntervalSeconds(kind string) int {
	if kind == "models" {
		return 86400
	}
	return 3600
}

// ensureJobs creates the missing lanes for one provider. Lanes the provider
// cannot serve are not created, so the UI never shows a job that can only fail
// with ErrUnsupported.
func (d *DB) ensureJobs(ctx context.Context, p Provider) error {
	preset, ok := provider.PresetFor(p.Kind)
	if !ok {
		return nil
	}
	caps := preset.Capabilities
	lanes := []struct {
		kind    string
		enabled bool
	}{
		{"models", caps.Models},
		{"usage", caps.Usage},
		{"costs", caps.Costs},
		{"balance", caps.Balance},
	}
	now := nowMS()
	for _, lane := range lanes {
		if !lane.enabled {
			continue
		}
		_, err := d.db.ExecContext(ctx, `INSERT INTO collector_jobs(
			id,provider_id,kind,enabled,interval_seconds,next_run_at_ms)
			VALUES(?,?,?,?,?,?) ON CONFLICT(provider_id,kind) DO NOTHING`,
			newID(), p.ID, lane.kind, 1, defaultIntervalSeconds(lane.kind), now)
		if err != nil {
			return wrap("seed jobs", err)
		}
	}
	return nil
}

// seedJobs repairs job rows for providers that predate this version.
func (d *DB) seedJobs(ctx context.Context) error {
	rows, err := d.db.QueryContext(ctx, "SELECT "+providerCols+" FROM providers")
	if err != nil {
		return wrap("seed jobs", err)
	}
	var all []Provider
	for rows.Next() {
		p, serr := scanProvider(rows)
		if serr != nil {
			rows.Close()
			return wrap("seed jobs", serr)
		}
		all = append(all, p)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return wrap("seed jobs", err)
	}
	for _, p := range all {
		if err = d.ensureJobs(ctx, p); err != nil {
			return err
		}
	}
	return nil
}
