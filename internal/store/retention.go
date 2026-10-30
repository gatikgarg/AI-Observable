package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const (
	metaRetentionRun     = "retention.last_run_ms"
	metaRetentionDeleted = "retention.deleted_rows"
)

// RecordImport writes one ledger row, replacing the previous result for the
// same provider, kind and window.
func (s *Store) RecordImport(ctx context.Context, imp ImportRow) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO provider_usage_imports(
		provider,kind,window_start_ms,window_end_ms,status,rows_imported,requests_imported,
		cost_imported_usd,error,started_at_ms,finished_at_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(provider,kind,window_start_ms,window_end_ms) DO UPDATE SET
			status=excluded.status, rows_imported=excluded.rows_imported,
			requests_imported=excluded.requests_imported, cost_imported_usd=excluded.cost_imported_usd,
			error=excluded.error, started_at_ms=excluded.started_at_ms,
			finished_at_ms=excluded.finished_at_ms`,
		imp.Provider, imp.Kind, imp.WindowStartMS, imp.WindowEndMS, imp.Status,
		imp.RowsImported, imp.RequestsImported, imp.CostImportedUSD, nullstr(imp.ErrText),
		imp.StartedAtMS, imp.FinishedAtMS)
	if err != nil {
		return 0, fmt.Errorf("store: record import: %w", err)
	}
	return res.LastInsertId()
}

// ImportRows returns the newest ledger rows, newest first.
func (s *Store) ImportRows(ctx context.Context, limit int) ([]ImportRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT provider,kind,window_start_ms,window_end_ms,status,
		rows_imported,requests_imported,cost_imported_usd,COALESCE(error,''),started_at_ms,finished_at_ms
		FROM provider_usage_imports ORDER BY started_at_ms DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ImportRow{}
	for rows.Next() {
		var r ImportRow
		var cost sql.NullFloat64
		var fin sql.NullInt64
		if err = rows.Scan(&r.Provider, &r.Kind, &r.WindowStartMS, &r.WindowEndMS, &r.Status,
			&r.RowsImported, &r.RequestsImported, &cost, &r.ErrText, &r.StartedAtMS, &fin); err != nil {
			return nil, err
		}
		if cost.Valid {
			v := cost.Float64
			r.CostImportedUSD = &v
		}
		if fin.Valid {
			v := fin.Int64
			r.FinishedAtMS = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Retention deletes calls older than days and records the run in meta. A
// non-positive window is a no-op.
func (s *Store) Retention(ctx context.Context, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -days).UnixMilli()
	res, err := s.DB.ExecContext(ctx, `DELETE FROM calls WHERE ts_ms < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err = s.SetMeta(ctx, metaRetentionRun, fmt.Sprint(time.Now().UnixMilli())); err != nil {
		return n, err
	}
	if err = s.SetMeta(ctx, metaRetentionDeleted, fmt.Sprint(n)); err != nil {
		return n, err
	}
	return n, nil
}

// RetentionStatus reports the last retention run and how many rows remain.
func (s *Store) RetentionStatus(ctx context.Context) (lastRun time.Time, rows int64, err error) {
	var last string
	if last, err = s.Meta(ctx, metaRetentionRun); err != nil {
		return time.Time{}, 0, err
	}
	if last != "" {
		var ms int64
		if _, scanErr := fmt.Sscan(last, &ms); scanErr == nil {
			lastRun = time.UnixMilli(ms).UTC()
		}
	}
	err = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM calls`).Scan(&rows)
	return lastRun, rows, err
}
