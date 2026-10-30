package admin

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ai-observable/ai-observable/internal/store"
)

// summaryView is store.Summary in the field names the Metrics tab reads.
type summaryView struct {
	FromMS    int64   `json:"from_ms"`
	ToMS      int64   `json:"to_ms"`
	Calls     int64   `json:"calls"`
	Failures  int64   `json:"failures"`
	Synthetic int64   `json:"synthetic"`
	CostUSD   float64 `json:"cost_usd"`

	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`

	CostReportedUSD float64            `json:"cost_reported_usd"`
	PricingMissing  int64              `json:"pricing_missing"`
	FillRate        map[string]float64 `json:"fill_rate"`
}

func (s *Server) usageSummary(w http.ResponseWriter, r *http.Request) {
	st := s.storeReady(w)
	if st == nil {
		return
	}
	from, to, err := queryWindow(r, 24*time.Hour)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	sum, err := st.Summary(r.Context(), store.SummaryQuery{
		From: from, To: to, Provider: queryString(r, "provider"), Model: queryString(r, "model"),
	})
	if err != nil {
		fail(w, err)
		return
	}
	ok(w, summaryView{
		FromMS: from, ToMS: to,
		Calls: sum.Calls, Failures: sum.Failures, Synthetic: sum.Synthetic,
		InputTokens: sum.InputTokens, OutputTokens: sum.OutputTokens,
		CacheReadTokens: sum.CacheReadTokens, CacheWriteTokens: sum.CacheWriteTokens,
		ReasoningTokens: sum.ReasoningTokens, CostUSD: sum.CostUSD,
		CostReportedUSD: sum.CostReportedUSD, PricingMissing: sum.PricingMissing,
		FillRate: sum.FillRate,
	})
}

// bucketView is one hour or day of measured traffic.
type bucketView struct {
	StartMS  int64   `json:"start_ms"`
	EndMS    int64   `json:"end_ms"`
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Calls    int64   `json:"calls"`
	Failures int64   `json:"failures"`
	Input    int64   `json:"input"`
	Output   int64   `json:"output"`
	CostUSD  float64 `json:"cost_usd"`
}

func (s *Server) usageBuckets(w http.ResponseWriter, r *http.Request) {
	st := s.storeReady(w)
	if st == nil {
		return
	}
	gran := strings.ToLower(queryString(r, "granularity"))
	if gran == "" {
		gran = "hour"
	}
	if gran != "hour" && gran != "day" {
		writeErr(w, http.StatusBadRequest, "bad_request", "granularity must be hour or day")
		return
	}
	from, to, err := queryWindow(r, 24*time.Hour)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	rows, err := st.UsageBuckets(r.Context(),
		store.Window{StartMS: from, EndMS: to, Granularity: gran},
		queryString(r, "provider"), queryString(r, "model"))
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]bucketView, 0, len(rows))
	for _, b := range rows {
		out = append(out, bucketView{
			StartMS: b.StartMS, EndMS: b.EndMS, Provider: b.Provider, Model: b.Model,
			Calls: b.Calls, Failures: b.Failures, Input: b.Input, Output: b.Output, CostUSD: b.CostUSD,
		})
	}
	ok(w, out)
}

// costView splits computed spend by line item and puts the provider-reported
// total beside it. DifferenceUSD is the measured gap; no cost is counted twice.
type costView struct {
	FromMS           int64   `json:"from_ms"`
	ToMS             int64   `json:"to_ms"`
	Calls            int64   `json:"calls"`
	CostUSD          float64 `json:"cost_usd"`
	CostInputUSD     float64 `json:"cost_input_usd"`
	CostCacheReadUSD float64 `json:"cost_cache_read_usd"`
	CostOutputUSD    float64 `json:"cost_output_usd"`
	CostOtherUSD     float64 `json:"cost_other_usd"`
	CostReportedUSD  float64 `json:"cost_reported_usd"`
	PricingMissing   int64   `json:"pricing_missing"`
	DifferenceUSD    float64 `json:"difference_usd"`
}

func (s *Server) costsSummary(w http.ResponseWriter, r *http.Request) {
	st := s.storeReady(w)
	if st == nil {
		return
	}
	from, to, err := queryWindow(r, 24*time.Hour)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	provider := queryString(r, "provider")
	sum, err := st.Summary(r.Context(), store.SummaryQuery{From: from, To: to, Provider: provider})
	if err != nil {
		fail(w, err)
		return
	}
	out := costView{
		FromMS: from, ToMS: to, Calls: sum.Calls, CostUSD: sum.CostUSD,
		CostReportedUSD: sum.CostReportedUSD, PricingMissing: sum.PricingMissing,
		DifferenceUSD: sum.CostUSD - sum.CostReportedUSD,
	}
	where := `WHERE ts_ms>=? AND ts_ms<=?`
	args := []any{from, to}
	if provider != "" {
		where += ` AND provider=?`
		args = append(args, provider)
	}
	err = st.DB.QueryRowContext(r.Context(), `SELECT
		COALESCE(SUM(cost_input_usd),0),COALESCE(SUM(cost_cache_read_usd),0),
		COALESCE(SUM(cost_output_usd),0) FROM calls `+where, args...).
		Scan(&out.CostInputUSD, &out.CostCacheReadUSD, &out.CostOutputUSD)
	if err != nil {
		fail(w, fmt.Errorf("cost split: %w", err))
		return
	}
	// Rounding can leave a tiny negative remainder; report 0, never -0.0000001.
	if other := out.CostUSD - (out.CostInputUSD + out.CostCacheReadUSD + out.CostOutputUSD); other > 0 {
		out.CostOtherUSD = other
	}
	ok(w, out)
}

// modelView is one catalog row. Prices stay nullable: a model without a price is
// visibly unpriced, not free.
type modelView struct {
	Provider     string   `json:"provider"`
	ModelID      string   `json:"model_id"`
	DisplayName  string   `json:"display_name,omitempty"`
	ContextMax   *int64   `json:"context_max,omitempty"`
	InputPrice   *float64 `json:"input_price,omitempty"`
	OutputPrice  *float64 `json:"output_price,omitempty"`
	CacheRead    *float64 `json:"cache_read_price,omitempty"`
	CacheWrite   *float64 `json:"cache_write_price,omitempty"`
	PriceSource  string   `json:"price_source,omitempty"`
	Capabilities string   `json:"capabilities,omitempty"`
	FirstSeenMS  int64    `json:"first_seen_ms"`
	LastSeenMS   int64    `json:"last_seen_ms"`
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	st := s.storeReady(w)
	if st == nil {
		return
	}
	q := `SELECT provider,model_id,COALESCE(display_name,''),context_max,
		input_price,output_price,cache_read_price,cache_write_price,
		COALESCE(price_source,''),COALESCE(capabilities,''),
		COALESCE(first_seen_ms,0),COALESCE(last_seen_ms,0)
		FROM model_catalog`
	args := []any{}
	if p := queryString(r, "provider"); p != "" {
		q += ` WHERE provider=?`
		args = append(args, p)
	}
	q += ` ORDER BY provider, model_id LIMIT ?`
	args = append(args, queryLimit(r, 200, 2000))
	rows, err := st.DB.QueryContext(r.Context(), q, args...)
	if err != nil {
		fail(w, fmt.Errorf("model catalog: %w", err))
		return
	}
	defer rows.Close()
	out := []modelView{}
	for rows.Next() {
		var m modelView
		var ctxMax sql.NullInt64
		var in, outp, cr, cw sql.NullFloat64
		if err = rows.Scan(&m.Provider, &m.ModelID, &m.DisplayName, &ctxMax,
			&in, &outp, &cr, &cw, &m.PriceSource, &m.Capabilities,
			&m.FirstSeenMS, &m.LastSeenMS); err != nil {
			fail(w, fmt.Errorf("model catalog: %w", err))
			return
		}
		if ctxMax.Valid {
			m.ContextMax = &ctxMax.Int64
		}
		for _, f := range []struct {
			dst **float64
			src sql.NullFloat64
		}{{&m.InputPrice, in}, {&m.OutputPrice, outp}, {&m.CacheRead, cr}, {&m.CacheWrite, cw}} {
			if f.src.Valid {
				v := f.src.Float64
				*f.dst = &v
			}
		}
		out = append(out, m)
	}
	if err = rows.Err(); err != nil {
		fail(w, fmt.Errorf("model catalog: %w", err))
		return
	}
	ok(w, out)
}
