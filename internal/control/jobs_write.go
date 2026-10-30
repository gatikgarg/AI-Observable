package control

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// UpsertJob creates or updates a lane. The cursor is preserved on update: only
// the collector advances it.
func (d *DB) UpsertJob(ctx context.Context, j Job) (Job, error) {
	p, err := d.GetProvider(ctx, j.ProviderID)
	if err != nil {
		return Job{}, err
	}
	valid := false
	for _, k := range JobKinds {
		if k == j.Kind {
			valid = true
		}
	}
	if !valid {
		return Job{}, invalid("job kind %q must be one of %v", j.Kind, JobKinds)
	}
	if j.IntervalSeconds <= 0 {
		j.IntervalSeconds = defaultIntervalSeconds(j.Kind)
	}
	if j.IntervalSeconds < 30 {
		return Job{}, invalid("interval_seconds must be at least 30")
	}
	if j.ID == "" {
		j.ID = newID()
	}
	_, err = d.db.ExecContext(ctx, `INSERT INTO collector_jobs(
		id,provider_id,kind,enabled,interval_seconds,next_run_at_ms)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(provider_id,kind) DO UPDATE SET
			enabled=excluded.enabled, interval_seconds=excluded.interval_seconds,
			next_run_at_ms=COALESCE(collector_jobs.next_run_at_ms, excluded.next_run_at_ms)`,
		j.ID, p.ID, j.Kind, boolInt(j.Enabled), j.IntervalSeconds, nowMS())
	if err != nil {
		return Job{}, wrap("upsert job", err)
	}
	return d.jobByProviderKind(ctx, p.ID, j.Kind)
}

func (d *DB) jobByProviderKind(ctx context.Context, providerID, kind string) (Job, error) {
	j, err := scanJob(d.db.QueryRowContext(ctx,
		"SELECT "+jobCols+" FROM collector_jobs WHERE provider_id=? AND kind=?", providerID, kind))
	if err != nil {
		return Job{}, wrap("get job", err)
	}
	return j, nil
}

// ListJobs returns the lanes for one provider, or every lane when providerID is
// empty.
func (d *DB) ListJobs(ctx context.Context, providerID string) ([]Job, error) {
	q := "SELECT " + jobCols + " FROM collector_jobs"
	args := []any{}
	if providerID != "" {
		p, err := d.GetProvider(ctx, providerID)
		if err != nil {
			return nil, err
		}
		q += " WHERE provider_id=?"
		args = append(args, p.ID)
	}
	q += " ORDER BY provider_id,kind"
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, wrap("list jobs", err)
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, serr := scanJob(rows)
		if serr != nil {
			return nil, wrap("list jobs", serr)
		}
		out = append(out, j)
	}
	return out, wrap("list jobs", rows.Err())
}

// UpdateJobRun records one run: status, error text, advanced cursor and the
// next due time. Consecutive failures drive the backoff.
func (d *DB) UpdateJobRun(ctx context.Context, id, status, errText string, cursor *int64, next int64) error {
	switch status {
	case "ok", "partial", "unsupported", "error", "skipped":
	default:
		return invalid("job status %q must be ok, partial, unsupported, error or skipped", status)
	}
	now := nowMS()
	failures := 0
	if status == "error" {
		failures = 1
	}
	res, err := d.db.ExecContext(ctx, `UPDATE collector_jobs SET last_run_at_ms=?,last_status=?,
		last_error=?,cursor_ms=COALESCE(?,cursor_ms),next_run_at_ms=?,
		consecutive_failures=CASE WHEN ?=1 THEN consecutive_failures+1 ELSE 0 END WHERE id=?`,
		now, status, nullIfEmpty(errText), cursor, next, failures, id)
	if err != nil {
		return wrap("update job run", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: job %q", ErrNotFound, id)
	}
	return nil
}

// DueJobs returns enabled lanes whose next run is at or before now.
func (d *DB) DueJobs(ctx context.Context, now int64) ([]Job, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+jobCols+` FROM collector_jobs
		WHERE enabled=1 AND (next_run_at_ms IS NULL OR next_run_at_ms<=?)
		ORDER BY next_run_at_ms`, now)
	if err != nil {
		return nil, wrap("due jobs", err)
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, serr := scanJob(rows)
		if serr != nil {
			return nil, wrap("due jobs", serr)
		}
		out = append(out, j)
	}
	return out, wrap("due jobs", rows.Err())
}

// GetJob returns one lane by id.
func (d *DB) GetJob(ctx context.Context, id string) (Job, error) {
	j, err := scanJob(d.db.QueryRowContext(ctx,
		"SELECT "+jobCols+" FROM collector_jobs WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("%w: job %q", ErrNotFound, id)
	}
	if err != nil {
		return Job{}, wrap("get job", err)
	}
	return j, nil
}
