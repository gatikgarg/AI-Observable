package dashboards

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ai-observable/ai-observable/internal/store"
	_ "modernc.org/sqlite"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestGrafanaFilesAreFrozen(t *testing.T) {
	root := repoRoot(t)
	f, err := os.Open("GRAFANA_SHA256SUMS")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	want := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			t.Fatalf("bad manifest line %q", sc.Text())
		}
		want[filepath.ToSlash(fields[1])] = fields[0]
	}
	if err = sc.Err(); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, dir := range []string{"deploy/grafana/dashboards", "deploy/grafana/provisioning"} {
		err = filepath.WalkDir(filepath.Join(root, filepath.FromSlash(dir)), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			got[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	compose := filepath.Join(root, "compose.yaml")
	b, err := os.ReadFile(compose)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	got["compose.yaml"] = hex.EncodeToString(sum[:])
	if fmt.Sprint(got) != fmt.Sprint(want) {
		keys := make([]string, 0, len(got)+len(want))
		for k, v := range got {
			keys = append(keys, k)
			if want[k] != v {
				t.Errorf("%s hash=%s, want %s", k, v, want[k])
			}
		}
		for k := range want {
			if _, ok := got[k]; !ok {
				keys = append(keys, k)
				t.Errorf("%s missing", k)
			}
		}
		sort.Strings(keys)
		t.Fatalf("Grafana files changed; restore them before merging (seen %v)", keys)
	}
}

func TestEveryDashboardQueryHasIdenticalResultsAcrossAdditiveMigration(t *testing.T) {
	root := repoRoot(t)
	queries := dashboardQueries(t, root)
	if len(queries) != 40 {
		t.Fatalf("found %d dashboard queries, want 40", len(queries))
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "legacy.db"))+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createLegacyDatabase(t, db)
	now := time.Now().UnixMilli()
	insertLegacyFixture(t, db, now)

	from, to := now-int64(time.Hour/time.Millisecond), now+int64(time.Hour/time.Millisecond)
	before := make([][][]string, len(queries))
	for i, q := range queries {
		before[i] = runDashboardQuery(t, db, q, from, to)
	}
	oldCols := viewColumns(t, db)
	if err = store.Migrate(db); err != nil {
		t.Fatalf("additive migration: %v", err)
	}
	newCols := viewColumns(t, db)
	if len(newCols) < len(oldCols) {
		t.Fatalf("view lost columns: before=%v after=%v", oldCols, newCols)
	}
	for i := range oldCols {
		if newCols[i] != oldCols[i] {
			t.Fatalf("existing view column %d changed: %q -> %q", i, oldCols[i], newCols[i])
		}
	}
	for i, q := range queries {
		after := runDashboardQuery(t, db, q, from, to)
		if fmt.Sprint(after) != fmt.Sprint(before[i]) {
			t.Fatalf("dashboard query %d changed across migration:\n%s\nbefore=%v\nafter=%v", i, q, before[i], after)
		}
	}
}

func dashboardQueries(t *testing.T, root string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "deploy", "grafana", "dashboards", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	var out []string
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var dash struct {
			Panels []struct {
				Targets []struct {
					RawQueryText string `json:"rawQueryText"`
				} `json:"targets"`
			} `json:"panels"`
		}
		if err = json.Unmarshal(b, &dash); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, panel := range dash.Panels {
			for _, target := range panel.Targets {
				if strings.TrimSpace(target.RawQueryText) != "" {
					out = append(out, target.RawQueryText)
				}
			}
		}
	}
	return out
}

func runDashboardQuery(t *testing.T, db *sql.DB, query string, from, to int64) [][]string {
	t.Helper()
	query = strings.ReplaceAll(query, "${model:raw}", "__all")
	query = strings.ReplaceAll(query, "${model:singlequote}", "'gpt-4o-mini'")
	query = strings.ReplaceAll(query, "$__from", strconv.FormatInt(from, 10))
	query = strings.ReplaceAll(query, "$__to", strconv.FormatInt(to, 10))
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("dashboard SQL failed: %v\n%s", err, query)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	out := [][]string{cols}
	for rows.Next() {
		raw := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err = rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		rec := make([]string, len(raw))
		for i, v := range raw {
			switch x := v.(type) {
			case nil:
				rec[i] = "<nil>"
			case []byte:
				rec[i] = string(x)
			case float64:
				rec[i] = strconv.FormatFloat(x, 'g', -1, 64)
			case int64:
				rec[i] = strconv.FormatInt(x, 10)
			default:
				rec[i] = fmt.Sprint(x)
			}
		}
		out = append(out, rec)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func viewColumns(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(calls_v)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var d any
		if err = rows.Scan(&cid, &name, &typ, &notnull, &d, &pk); err != nil {
			t.Fatal(err)
		}
		// SQLite often reports no declared type for computed view columns. The
		// dashboard contract is the column set; values are compared below.
		out = append(out, name)
	}
	return out
}

func createLegacyDatabase(t *testing.T, db *sql.DB) {
	t.Helper()
	statements := []string{
		`CREATE TABLE calls (id INTEGER PRIMARY KEY,ts_ms INTEGER NOT NULL,source TEXT NOT NULL DEFAULT 'proxy',provider TEXT NOT NULL,endpoint TEXT NOT NULL,model_requested TEXT,model_served TEXT,stream INTEGER NOT NULL DEFAULT 0,status INTEGER,success INTEGER NOT NULL,error_type TEXT,error_message TEXT,latency_ms INTEGER NOT NULL,ttft_ms INTEGER,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_write_tokens INTEGER,reasoning_tokens INTEGER,usage_estimated INTEGER NOT NULL DEFAULT 0,cost_usd REAL,pricing_missing INTEGER NOT NULL DEFAULT 0,context_max INTEGER,context_used_pct REAL,session_id TEXT,key_fingerprint TEXT,request_preview TEXT,response_preview TEXT,finish_reason TEXT,request_bytes INTEGER,response_bytes INTEGER,project TEXT,request_id TEXT,trace_id TEXT,attempt INTEGER NOT NULL DEFAULT 1,cost_input_usd REAL,cost_cache_read_usd REAL,cost_output_usd REAL)`,
		`CREATE VIEW calls_v AS SELECT id,CAST(ts_ms/1000 AS INTEGER) AS ts,provider,endpoint,COALESCE(model_served,model_requested,'unknown') AS model,stream,status,success,error_type,error_message,latency_ms,ttft_ms,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,COALESCE(input_tokens,0)+COALESCE(output_tokens,0) AS total_tokens,usage_estimated,cost_usd,pricing_missing,context_max,context_used_pct,session_id,key_fingerprint,request_preview,response_preview,finish_reason,request_bytes,response_bytes,source,project,request_id,trace_id,attempt,cost_input_usd,cost_cache_read_usd,cost_output_usd,MAX(0,COALESCE(cost_usd,0)-COALESCE(cost_input_usd,0)-COALESCE(cost_cache_read_usd,0)-COALESCE(cost_output_usd,0)) AS cost_other_usd,CASE WHEN COALESCE(error_type,'')='client_cancelled' OR COALESCE(status,-1)=499 THEN 1 ELSE 0 END AS is_cancelled,CASE WHEN success=0 AND NOT (COALESCE(error_type,'')='client_cancelled' OR COALESCE(status,-1)=499) THEN 1 ELSE 0 END AS is_failure,CASE WHEN success=1 THEN NULL WHEN COALESCE(error_type,'')='client_cancelled' OR COALESCE(status,-1)=499 THEN 'client_cancelled' WHEN COALESCE(error_type,'')='timeout' THEN 'timeout' WHEN COALESCE(status,-1)=0 OR COALESCE(error_type,'')='connection_error' THEN 'connection_error' WHEN COALESCE(error_type,'')='invalid_response' THEN 'invalid_response' WHEN COALESCE(status,-1)=429 THEN 'http_429' WHEN COALESCE(status,-1) BETWEEN 400 AND 499 THEN 'http_4xx' WHEN COALESCE(status,-1) BETWEEN 500 AND 599 THEN 'http_5xx' ELSE 'other' END AS error_category FROM calls`,
	}
	for _, q := range statements {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("legacy schema: %v", err)
		}
	}
}

func insertLegacyFixture(t *testing.T, db *sql.DB, now int64) {
	t.Helper()
	q := `INSERT INTO calls(ts_ms,provider,endpoint,model_requested,model_served,stream,status,success,
		error_type,error_message,latency_ms,ttft_ms,input_tokens,output_tokens,cache_read_tokens,
		cache_write_tokens,reasoning_tokens,cost_usd,pricing_missing,session_id,project,request_id,
		trace_id,attempt,cost_input_usd,cost_cache_read_usd,cost_output_usd)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	rows := []struct {
		dt                                               int64
		provider, model, errorType, project, trace, rid  string
		stream, status, success                          int
		latency, ttft, input, output, cacheRead, attempt int64
		cost                                             float64
		priced                                           int
	}{
		{-60_000, "openai", "gpt-4o-mini", "", "alpha", "trace-a", "req-a", 1, 200, 1, 900, 120, 100, 30, 12, 0, 0.001, 1},
		{-50_000, "openai", "gpt-4o-mini", "", "beta", "", "", 0, 200, 1, 400, 0, 200, 20, 50, 1, 0.002, 1},
		{-40_000, "deepseek", "deepseek-flash", "rate_limit", "alpha", "trace-b", "", 1, 429, 0, 1800, 300, 300, 0, 0, 1, 0, 0},
		{-30_000, "deepseek", "deepseek-flash", "", "gamma", "trace-c", "", 1, 200, 1, 2600, 500, 800, 120, 40, 2, 0.0045, 1},
		{-20_000, "openai", "gpt-4o", "", "delta", "trace-d", "", 0, 500, 0, 7000, 50, 50, 0, 0, 1, 0, 1},
		{-10_000, "openai", "gpt-4o-mini", "client_cancelled", "delta", "trace-e", "", 1, 499, 0, 5000, 80, 60, 0, 0, 1, 0, 1},
	}
	for _, r := range rows {
		if _, err := db.Exec(q, now+r.dt, r.provider, "v1/chat/completions", r.model, r.model,
			r.stream, r.status, r.success, r.errorType, "", r.latency, r.ttft, r.input, r.output,
			r.cacheRead, 0, 0, r.cost, 1-r.priced, "session-"+r.project, r.project, r.rid, r.trace,
			r.attempt, r.cost/2, 0, r.cost/2); err != nil {
			t.Fatal(err)
		}
	}
}
