package store

import (
	"context"
	"database/sql"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type CallRecord struct {
	TS                                                                    int64
	Source, Provider, Endpoint, ModelRequested, ModelServed               string
	Stream                                                                bool
	Status                                                                int
	Success                                                               bool
	ErrorType, ErrorMessage                                               string
	Latency, TTFT                                                         int64
	Input, Output, CacheRead, CacheWrite, Reasoning                       *int64
	Estimated                                                             bool
	Cost, CostInput, CostCache, CostOutput                                *float64
	PricingMissing                                                        bool
	ContextMax                                                            *int64
	ContextPct                                                            *float64
	SessionID, Fingerprint, RequestPreview, ResponsePreview, FinishReason string
	Project, RequestID, TraceID                                           string
	IntegrationID, AgentType                                              string
	Attempt                                                               int
	RequestBytes, ResponseBytes                                           int64
}
type Store struct {
	DB          *sql.DB
	q           chan CallRecord
	dropped     atomic.Int64
	lastDropLog atomic.Int64
	dropLog     func(string)
	wg          sync.WaitGroup
	mu          sync.RWMutex
	closed      bool
	// ReconcileMode is wired by the collector to the control DB so store never
	// imports control. A nil hook reconciles nothing.
	ReconcileMode func(ctx context.Context, provider string) (string, error)
}

func Open(dir string) (*Store, error) {
	if e := os.MkdirAll(dir, 0755); e != nil {
		return nil, e
	}
	// DSN pragmas apply to every pooled connection (R15); a bare PRAGMA exec only
	// configures the one connection that happened to run it.
	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, "llmtap.db")) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, e := sql.Open("sqlite", dsn)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(8)
	if e = migrate(db, false); e != nil {
		db.Close()
		return nil, e
	}
	s := &Store{DB: db, q: make(chan CallRecord, 4096)}
	s.wg.Add(1)
	go s.writer()
	return s, nil
}

func migrate(db *sql.DB, rolling bool) error {
	for _, q := range []string{`CREATE TABLE IF NOT EXISTS schema_version(version INTEGER NOT NULL)`, `CREATE TABLE IF NOT EXISTS calls (id INTEGER PRIMARY KEY,ts_ms INTEGER NOT NULL,source TEXT NOT NULL DEFAULT 'proxy',provider TEXT NOT NULL,endpoint TEXT NOT NULL,model_requested TEXT,model_served TEXT,stream INTEGER NOT NULL DEFAULT 0,status INTEGER,success INTEGER NOT NULL,error_type TEXT,error_message TEXT,latency_ms INTEGER NOT NULL,ttft_ms INTEGER,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_write_tokens INTEGER,reasoning_tokens INTEGER,usage_estimated INTEGER NOT NULL DEFAULT 0,cost_usd REAL,pricing_missing INTEGER NOT NULL DEFAULT 0,context_max INTEGER,context_used_pct REAL,session_id TEXT,key_fingerprint TEXT,request_preview TEXT,response_preview TEXT,finish_reason TEXT,request_bytes INTEGER,response_bytes INTEGER)`, `CREATE INDEX IF NOT EXISTS idx_calls_ts ON calls(ts_ms)`, `CREATE INDEX IF NOT EXISTS idx_calls_model_ts ON calls(model_served,ts_ms)`, `CREATE INDEX IF NOT EXISTS idx_calls_session ON calls(session_id,ts_ms)`} {
		if _, e := db.Exec(q); e != nil {
			return e
		}
	}
	cols := map[string]string{"project": "TEXT", "request_id": "TEXT", "trace_id": "TEXT", "attempt": "INTEGER NOT NULL DEFAULT 1", "cost_input_usd": "REAL", "cost_cache_read_usd": "REAL", "cost_output_usd": "REAL", "integration_id": "TEXT", "agent_type": "TEXT"}
	rows, e := db.Query("PRAGMA table_info(calls)")
	if e != nil {
		return e
	}
	existing := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var d any
		rows.Scan(&cid, &name, &typ, &notnull, &d, &pk)
		existing[name] = true
	}
	rows.Close()
	for name, typ := range cols {
		if !existing[name] {
			if _, e = db.Exec("ALTER TABLE calls ADD COLUMN " + name + " " + typ); e != nil {
				return e
			}
		}
	}
	if _, e = db.Exec("CREATE INDEX IF NOT EXISTS idx_calls_trace ON calls(trace_id,attempt)"); e != nil {
		return e
	}
	if _, e = db.Exec("CREATE INDEX IF NOT EXISTS idx_calls_integration_ts ON calls(integration_id,ts_ms)"); e != nil {
		return e
	}
	if e = migrateV2(db); e != nil {
		return e
	}
	return recreateView(db, rolling)
}
func recreateView(db *sql.DB, rolling bool) error {
	if _, e := db.Exec("DROP VIEW IF EXISTS calls_v"); e != nil {
		return e
	}
	shift := ""
	if rolling {
		shift = " + (strftime('%s','now') - (SELECT COALESCE(MAX(ts_ms)/1000,strftime('%s','now')) FROM calls))"
	}
	q := `CREATE VIEW calls_v AS SELECT id,CAST(ts_ms/1000 AS INTEGER)` + shift + ` AS ts,provider,endpoint,COALESCE(NULLIF(model_served,''),NULLIF(model_requested,''),'unknown') AS model,stream,status,success,error_type,error_message,latency_ms,ttft_ms,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,COALESCE(input_tokens,0)+COALESCE(output_tokens,0) AS total_tokens,usage_estimated,cost_usd,pricing_missing,context_max,context_used_pct,session_id,key_fingerprint,request_preview,response_preview,finish_reason,request_bytes,response_bytes,source,project,request_id,trace_id,attempt,cost_input_usd,cost_cache_read_usd,cost_output_usd,MAX(0,COALESCE(cost_usd,0)-COALESCE(cost_input_usd,0)-COALESCE(cost_cache_read_usd,0)-COALESCE(cost_output_usd,0)) AS cost_other_usd,CASE WHEN COALESCE(error_type,'')='client_cancelled' OR COALESCE(status,-1)=499 THEN 1 ELSE 0 END AS is_cancelled,CASE WHEN success=0 AND NOT (COALESCE(error_type,'')='client_cancelled' OR COALESCE(status,-1)=499) THEN 1 ELSE 0 END AS is_failure,CASE WHEN success=1 THEN NULL WHEN COALESCE(error_type,'')='client_cancelled' OR COALESCE(status,-1)=499 THEN 'client_cancelled' WHEN COALESCE(error_type,'')='timeout' THEN 'timeout' WHEN COALESCE(status,-1)=0 OR COALESCE(error_type,'')='connection_error' THEN 'connection_error' WHEN COALESCE(error_type,'')='invalid_response' THEN 'invalid_response' WHEN COALESCE(status,-1)=429 THEN 'http_429' WHEN COALESCE(status,-1) BETWEEN 400 AND 499 THEN 'http_4xx' WHEN COALESCE(status,-1) BETWEEN 500 AND 599 THEN 'http_5xx' ELSE 'other' END AS error_category,integration_id,agent_type FROM calls`
	_, e := db.Exec(q)
	return e
}
func MakeRolling(db *sql.DB) error { return migrate(db, true) }

// Migrate applies the additive schema and view updates without shifting demo
// timestamps. It is used by upgrade and golden-query tests.
func Migrate(db *sql.DB) error { return migrate(db, false) }
func (s *Store) Record(r CallRecord) (accepted bool) {
	select {
	case s.q <- r:
		return true
	default:
		s.dropped.Add(1)
		now := time.Now().UnixNano()
		prev := s.lastDropLog.Load()
		if prev == 0 || now-prev >= int64(time.Second) {
			if s.lastDropLog.CompareAndSwap(prev, now) {
				msg := "llmtap writer queue full; call record dropped (the proxied request is unaffected)"
				if s.dropLog != nil {
					s.dropLog(msg)
				} else {
					fmt.Fprintln(os.Stderr, msg)
				}
			}
		}
		return false
	}
}

// RecordOK blocks until the writer accepts the record and reports false when
// the store is already closed, so a finished run is fully persisted.
func (s *Store) RecordOK(r CallRecord) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return false
	}
	s.q <- r
	return true
}
func (s *Store) writer() {
	defer s.wg.Done()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	batch := make([]CallRecord, 0, 100)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		tx, e := s.DB.Begin()
		if e == nil {
			st, e := tx.Prepare(`INSERT INTO calls(ts_ms,source,provider,endpoint,model_requested,model_served,stream,status,success,error_type,error_message,latency_ms,ttft_ms,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,usage_estimated,cost_usd,pricing_missing,context_max,context_used_pct,session_id,key_fingerprint,request_preview,response_preview,finish_reason,request_bytes,response_bytes,project,request_id,trace_id,attempt,cost_input_usd,cost_cache_read_usd,cost_output_usd,integration_id,agent_type) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
			if e == nil {
				for _, r := range batch {
					_, e = st.Exec(r.TS, r.Source, r.Provider, r.Endpoint, r.ModelRequested, r.ModelServed, r.Stream, r.Status, r.Success, r.ErrorType, r.ErrorMessage, r.Latency, nullable(r.TTFT), r.Input, r.Output, r.CacheRead, r.CacheWrite, r.Reasoning, r.Estimated, r.Cost, r.PricingMissing, r.ContextMax, r.ContextPct, r.SessionID, r.Fingerprint, r.RequestPreview, r.ResponsePreview, r.FinishReason, r.RequestBytes, r.ResponseBytes, nullstr(r.Project), nullstr(r.RequestID), nullstr(r.TraceID), attempt(r.Attempt), r.CostInput, r.CostCache, r.CostOutput, nullstr(r.IntegrationID), nullstr(r.AgentType))
					if e != nil {
						break
					}
				}
				st.Close()
			}
			if e == nil {
				e = tx.Commit()
			} else {
				tx.Rollback()
			}
			if e != nil {
				fmt.Fprintln(os.Stderr, "llmtap writer:", e)
				s.dropped.Add(int64(len(batch)))
			}
		}
		batch = batch[:0]
	}
	for {
		select {
		case r, ok := <-s.q:
			if !ok {
				flush()
				return
			}
			batch = append(batch, r)
			if len(batch) >= 100 {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}
func nullable(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
func nullstr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func attempt(n int) int {
	if n < 1 {
		return 1
	}
	if n > 50 {
		return 50
	}
	return n
}
func (s *Store) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.q)
	s.mu.Unlock()
	s.wg.Wait()
	s.DB.Close()
}
func (s *Store) Dropped() int64 { return s.dropped.Load() }
func (s *Store) Count(ctx context.Context) (int64, error) {
	var n int64
	e := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM calls").Scan(&n)
	return n, e
}
func (s *Store) Retain(days int) error {
	_, e := s.DB.Exec("DELETE FROM calls WHERE ts_ms < ?", time.Now().AddDate(0, 0, -days).UnixMilli())
	return e
}
