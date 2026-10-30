package admin

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// sessionView is one distinct session_id in the analytics database. Sessions are
// derived by the proxy, so this is a read-only rollup, not a stored entity.
type sessionView struct {
	SessionID    string  `json:"session_id"`
	Calls        int64   `json:"calls"`
	Failures     int64   `json:"failures"`
	Models       int64   `json:"models"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	FirstSeenMS  int64   `json:"first_seen_ms"`
	LastSeenMS   int64   `json:"last_seen_ms"`
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	st := s.storeReady(w)
	if st == nil {
		return
	}
	from, to, err := queryWindow(r, 7*24*time.Hour)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	q := `SELECT COALESCE(NULLIF(session_id,''),'(unattributed)') AS sid,
		COUNT(*),
		COALESCE(SUM(CASE WHEN success=0 AND NOT (COALESCE(error_type,'')='client_cancelled' OR COALESCE(status,-1)=499) THEN 1 ELSE 0 END),0),
		COUNT(DISTINCT COALESCE(NULLIF(model_served,''),model_requested,'unknown')),
		COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),
		COALESCE(SUM(cost_usd),0),MIN(ts_ms),MAX(ts_ms)
		FROM calls WHERE ts_ms>=? AND ts_ms<=?`
	args := []any{from, to}
	if p := queryString(r, "provider"); p != "" {
		q += ` AND provider=?`
		args = append(args, p)
	}
	q += ` GROUP BY sid ORDER BY MAX(ts_ms) DESC LIMIT ?`
	args = append(args, queryLimit(r, 50, 500))
	rows, err := st.DB.QueryContext(r.Context(), q, args...)
	if err != nil {
		fail(w, fmt.Errorf("sessions: %w", err))
		return
	}
	defer rows.Close()
	out := []sessionView{}
	for rows.Next() {
		var v sessionView
		if err = rows.Scan(&v.SessionID, &v.Calls, &v.Failures, &v.Models,
			&v.InputTokens, &v.OutputTokens, &v.CostUSD, &v.FirstSeenMS, &v.LastSeenMS); err != nil {
			fail(w, fmt.Errorf("sessions: %w", err))
			return
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		fail(w, fmt.Errorf("sessions: %w", err))
		return
	}
	ok(w, out)
}

// callView is one row of the Calls table. Token and cost fields stay pointers so
// "not reported" is distinct from zero in the JSON.
type callView struct {
	ID             int64    `json:"id"`
	TSMS           int64    `json:"ts_ms"`
	Source         string   `json:"source"`
	Provider       string   `json:"provider"`
	Endpoint       string   `json:"endpoint"`
	Model          string   `json:"model"`
	Stream         bool     `json:"stream"`
	Status         *int64   `json:"status,omitempty"`
	Success        bool     `json:"success"`
	ErrorType      string   `json:"error_type,omitempty"`
	ErrorMessage   string   `json:"error_message,omitempty"`
	LatencyMS      int64    `json:"latency_ms"`
	TTFTMS         *int64   `json:"ttft_ms,omitempty"`
	InputTokens    *int64   `json:"input_tokens,omitempty"`
	OutputTokens   *int64   `json:"output_tokens,omitempty"`
	CacheRead      *int64   `json:"cache_read_tokens,omitempty"`
	CacheWrite     *int64   `json:"cache_write_tokens,omitempty"`
	Reasoning      *int64   `json:"reasoning_tokens,omitempty"`
	UsageEstimated bool     `json:"usage_estimated"`
	CostUSD        *float64 `json:"cost_usd,omitempty"`
	PricingMissing bool     `json:"pricing_missing"`
	SessionID      string   `json:"session_id,omitempty"`
	KeyFingerprint string   `json:"key_fingerprint,omitempty"`
	Project        string   `json:"project,omitempty"`
	RequestID      string   `json:"request_id,omitempty"`
	TraceID        string   `json:"trace_id,omitempty"`
	Attempt        int64    `json:"attempt"`
	FinishReason   string   `json:"finish_reason,omitempty"`
	RequestText    string   `json:"request_preview,omitempty"`
	ResponseText   string   `json:"response_preview,omitempty"`
}

// callRow holds the nullable scan targets for one calls row.
type callRow struct {
	id, ts                                            int64
	source, provider, endpoint, modelReq, modelServed string
	stream, success, estimated, pricingMissing        int64
	status                                            sql.NullInt64
	errorType, errorMessage                           sql.NullString
	latency                                           int64
	ttft                                              sql.NullInt64
	input, output, cacheRead, cacheWrite, reasoning   sql.NullInt64
	cost, costIn, costCacheRead, costOut              sql.NullFloat64
	session, fingerprint, project, requestID, traceID sql.NullString
	done, requestText, responseText                   sql.NullString
	attempt                                           sql.NullInt64
}

// callColumns is the projection shared by the JSON list and the CSV export, so
// the two can never disagree about what a call contains.
const callColumns = `id,ts_ms,source,provider,endpoint,COALESCE(model_requested,''),
	COALESCE(model_served,''),stream,status,success,COALESCE(error_type,''),
	COALESCE(error_message,''),latency_ms,ttft_ms,input_tokens,output_tokens,
	cache_read_tokens,cache_write_tokens,reasoning_tokens,usage_estimated,cost_usd,
	pricing_missing,cost_input_usd,cost_cache_read_usd,cost_output_usd,session_id,
	key_fingerprint,project,request_id,trace_id,attempt,finish_reason,request_preview,response_preview`

func (c *callRow) dest() []any {
	return []any{&c.id, &c.ts, &c.source, &c.provider, &c.endpoint, &c.modelReq,
		&c.modelServed, &c.stream, &c.status, &c.success, &c.errorType, &c.errorMessage,
		&c.latency, &c.ttft, &c.input, &c.output, &c.cacheRead, &c.cacheWrite, &c.reasoning,
		&c.estimated, &c.cost, &c.pricingMissing, &c.costIn, &c.costCacheRead, &c.costOut,
		&c.session, &c.fingerprint, &c.project, &c.requestID, &c.traceID, &c.attempt,
		&c.done, &c.requestText, &c.responseText}
}

func (c *callRow) view() callView {
	v := callView{
		ID: c.id, TSMS: c.ts, Source: c.source, Provider: c.provider, Endpoint: c.endpoint,
		Model: c.modelServed, Stream: c.stream != 0, Success: c.success != 0,
		ErrorType: c.errorType.String, ErrorMessage: c.errorMessage.String,
		LatencyMS: c.latency, UsageEstimated: c.estimated != 0,
		PricingMissing: c.pricingMissing != 0, SessionID: c.session.String,
		KeyFingerprint: c.fingerprint.String, Project: c.project.String,
		RequestID: c.requestID.String, TraceID: c.traceID.String, Attempt: c.attempt.Int64,
		FinishReason: c.done.String, RequestText: c.requestText.String,
		ResponseText: c.responseText.String,
	}
	if v.Model == "" {
		v.Model = c.modelReq
	}
	if v.Attempt == 0 {
		v.Attempt = 1
	}
	if c.status.Valid {
		v.Status = &c.status.Int64
	}
	if c.ttft.Valid {
		v.TTFTMS = &c.ttft.Int64
	}
	for _, f := range []struct {
		dst **int64
		src sql.NullInt64
	}{{&v.InputTokens, c.input}, {&v.OutputTokens, c.output},
		{&v.CacheRead, c.cacheRead}, {&v.CacheWrite, c.cacheWrite}, {&v.Reasoning, c.reasoning}} {
		if f.src.Valid {
			n := f.src.Int64
			*f.dst = &n
		}
	}
	if c.cost.Valid {
		f := c.cost.Float64
		v.CostUSD = &f
	}
	return v
}

// callsQuery builds the filtered, newest-first query both routes use.
func (s *Server) callsQuery(r *http.Request, from, to int64) (string, []any) {
	q := `SELECT ` + callColumns + ` FROM calls WHERE ts_ms>=? AND ts_ms<=?`
	args := []any{from, to}
	if p := queryString(r, "provider"); p != "" {
		q += ` AND provider=?`
		args = append(args, p)
	}
	switch strings.ToLower(queryString(r, "status")) {
	case "", "all":
	case "ok", "success":
		q += ` AND success=1`
	case "error", "failed", "failure":
		q += ` AND success=0 AND NOT (COALESCE(error_type,'')='client_cancelled' OR COALESCE(status,-1)=499)`
	case "cancelled", "canceled":
		q += ` AND (COALESCE(error_type,'')='client_cancelled' OR COALESCE(status,-1)=499)`
	default:
		if n, err := strconv.Atoi(queryString(r, "status")); err == nil {
			q += ` AND status=?`
			args = append(args, n)
		} else {
			q += ` AND 0`
		}
	}
	return q, args
}

// recentCalls answers the Calls tab. With no from/to it means "the newest rows
// that exist", not "the last day": an empty window would look like a broken
// proxy on a quiet machine.
func (s *Server) recentCalls(w http.ResponseWriter, r *http.Request) {
	st := s.storeReady(w)
	if st == nil {
		return
	}
	from, to, err := recentWindow(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	q, args := s.callsQuery(r, from, to)
	q += ` ORDER BY ts_ms DESC, id DESC LIMIT ?`
	args = append(args, queryLimit(r, 50, 1000))
	rows, err := st.DB.QueryContext(r.Context(), q, args...)
	if err != nil {
		fail(w, fmt.Errorf("calls: %w", err))
		return
	}
	defer rows.Close()
	out := []callView{}
	for rows.Next() {
		var c callRow
		if err = rows.Scan(c.dest()...); err != nil {
			fail(w, fmt.Errorf("calls: %w", err))
			return
		}
		out = append(out, c.view())
	}
	if err = rows.Err(); err != nil {
		fail(w, fmt.Errorf("calls: %w", err))
		return
	}
	ok(w, out)
}

// recentWindow leaves the range open unless the caller narrows it.
func recentWindow(r *http.Request) (from, to int64, err error) {
	if queryString(r, "from") == "" && queryString(r, "to") == "" {
		return 0, math.MaxInt64, nil
	}
	return queryWindow(r, 24*time.Hour)
}

// csvHeader is the export's column order; previews come last and are already
// redacted in the database.
var csvHeader = []string{"ts_ms", "ts_utc", "source", "provider", "endpoint", "model",
	"stream", "status", "success", "error_type", "latency_ms", "ttft_ms", "input_tokens",
	"output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens",
	"usage_estimated", "cost_usd", "pricing_missing", "session_id", "project",
	"request_id", "trace_id", "attempt", "finish_reason", "request_preview", "response_preview"}

// exportCalls streams CSV so a large window never buffers in memory. It respects
// the same window and filters as the Calls tab, so what the user sees is what
// they download.
func (s *Server) exportCalls(w http.ResponseWriter, r *http.Request) {
	st := s.storeReady(w)
	if st == nil {
		return
	}
	from, to, err := recentWindow(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	q, args := s.callsQuery(r, from, to)
	q += ` ORDER BY ts_ms DESC, id DESC LIMIT ?`
	args = append(args, queryLimit(r, 10000, 200000))
	rows, err := st.DB.QueryContext(r.Context(), q, args...)
	if err != nil {
		fail(w, fmt.Errorf("export: %w", err))
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=\"llmtap-calls-%s.csv\"", time.Now().UTC().Format("20060102-150405")))
	cw := csv.NewWriter(w)
	if err = cw.Write(csvHeader); err != nil {
		return
	}
	n := 0
	for rows.Next() {
		var c callRow
		if err = rows.Scan(c.dest()...); err != nil {
			// Headers are already sent, so report in the log instead of a JSON
			// error the browser would download as CSV text.
			s.Logs.Add("error", "csv export failed", "error", err.Error())
			return
		}
		if err = cw.Write(c.csvRecord()); err != nil {
			return
		}
		n++
		if n%500 == 0 {
			cw.Flush()
		}
	}
	if err = rows.Err(); err != nil {
		s.Logs.Add("error", "csv export stopped early", "error", err.Error())
		return
	}
	cw.Flush()
	s.Logs.Add("info", "calls exported", "rows", strconv.Itoa(n), "from", strconv.FormatInt(from, 10),
		"to", strconv.FormatInt(to, 10))
}

// csvRecord renders one call. Absent numbers stay empty: a blank cell means the
// provider did not report it, which is not the same as zero.
func (c *callRow) csvRecord() []string {
	rec := []string{
		strconv.FormatInt(c.ts, 10),
		time.UnixMilli(c.ts).UTC().Format(time.RFC3339),
		c.source, c.provider, c.endpoint,
		c.view().Model,
		boolCell(c.stream != 0), intCell(c.status), boolCell(c.success != 0),
		c.errorType.String, strconv.FormatInt(c.latency, 10), intCell(c.ttft),
		intCell(c.input), intCell(c.output), intCell(c.cacheRead), intCell(c.cacheWrite),
		intCell(c.reasoning), boolCell(c.estimated != 0), floatCell(c.cost),
		boolCell(c.pricingMissing != 0), c.session.String, c.project.String,
		c.requestID.String, c.traceID.String,
		intCell(sql.NullInt64{Int64: c.attempt.Int64, Valid: c.attempt.Valid}),
		c.done.String, c.requestText.String, c.responseText.String,
	}
	return rec
}

func boolCell(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func intCell(v sql.NullInt64) string {
	if !v.Valid {
		return ""
	}
	return strconv.FormatInt(v.Int64, 10)
}

func floatCell(v sql.NullFloat64) string {
	if !v.Valid {
		return ""
	}
	return strconv.FormatFloat(v.Float64, 'f', -1, 64)
}
