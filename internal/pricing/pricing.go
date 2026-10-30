// Package pricing resolves per-model token prices.
//
// Precedence: the user file (~/.llmtap/prices.json, optional), then the
// embedded versioned prices.json, then the compiled-in fallback table. Nothing
// is fetched from the network.
package pricing

import (
	"strings"
	"sync"
	"time"
)

// Price is the USD cost of one token in each direction.
type Price struct {
	Input, Output, CacheRead, CacheWrite float64
	Context                              int64
}

// fallback is the compiled-in table. It stays as the last resort so a broken
// or missing prices.json can never make every call "pricing_missing".
var fallback = map[string]Price{
	"gpt-4o":            {Input: 2.5e-6, Output: 10e-6, CacheRead: 1.25e-6, Context: 128000},
	"gpt-4o-mini":       {Input: 0.15e-6, Output: 0.60e-6, CacheRead: 0.075e-6, Context: 128000},
	"gpt-4.1":           {Input: 2e-6, Output: 8e-6, CacheRead: 0.5e-6, Context: 1047576},
	"gpt-4.1-mini":      {Input: 0.4e-6, Output: 1.6e-6, CacheRead: 0.1e-6, Context: 1047576},
	"o3":                {Input: 2e-6, Output: 8e-6, CacheRead: 0.5e-6, Context: 200000},
	"o4-mini":           {Input: 1.1e-6, Output: 4.4e-6, CacheRead: 0.275e-6, Context: 200000},
	"deepseek-chat":     {Input: 0.27e-6, Output: 1.1e-6, CacheRead: 0.07e-6, Context: 64000},
	"deepseek-reasoner": {Input: 0.55e-6, Output: 2.19e-6, CacheRead: 0.14e-6, Context: 64000},
	// Verified 2026-10-28, https://api-docs.deepseek.com/quick_start/pricing.
	// DeepSeek prices vary by peak/off-peak time; these use the published peak
	// rates so locally computed spend is an estimate and never understates it.
	"deepseek-flash":          {Input: 0.30e-6, Output: 1.20e-6, CacheRead: 0.006e-6, Context: 1000000},
	"deepseek-v4-flash":       {Input: 0.30e-6, Output: 1.20e-6, CacheRead: 0.006e-6, Context: 1000000},
	"deepseek-v4-pro":         {Input: 1.32e-6, Output: 3.96e-6, CacheRead: 0.044e-6, Context: 1000000},
	"llama-3.3-70b-versatile": {Input: 0.59e-6, Output: 0.79e-6, Context: 128000},
	"claude-3-5-sonnet":       {Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6, Context: 200000},
}

var (
	mu        sync.RWMutex
	embedded  = map[string]Price{}
	overrides = map[string]Price{}
	status    = Status{Version: "compiled-in"}
)

// Status describes the active price source for /api/pricing/status.
type Status struct {
	Path      string    `json:"path"`
	ModTime   time.Time `json:"mod_time"`
	Models    int       `json:"models"`
	Version   string    `json:"version"`
	LastError string    `json:"last_error"`
}

// Lookup resolves a model name, tolerating a "provider/model" prefix and a
// dated suffix such as "gpt-4o-2024-08-06".
func Lookup(model string) (Price, bool) {
	if model == "" {
		return Price{}, false
	}
	if i := strings.LastIndex(model, "/"); i >= 0 {
		if p, ok := Lookup(model[i+1:]); ok {
			return p, true
		}
	}
	mu.RLock()
	if p, ok := overrides[model]; ok {
		mu.RUnlock()
		return p, true
	}
	if p, ok := embedded[model]; ok {
		mu.RUnlock()
		return p, true
	}
	mu.RUnlock()
	if p, ok := fallback[model]; ok {
		return p, true
	}
	var best Price
	bestLen := 0
	for key, p := range fallback {
		if len(key) > bestLen && strings.HasPrefix(model, key) && isVersionSeparator(model[len(key)]) {
			best, bestLen = p, len(key)
		}
	}
	if bestLen > 0 {
		return best, true
	}
	return Price{}, false
}

func isVersionSeparator(b byte) bool { return b == '-' || b == ':' || b == '@' }

// Breakdown is the cost split written into calls.cost_*_usd.
type Breakdown struct {
	InputUncached, CacheRead, CacheWrite, Output, Total float64
}

// CostBreakdown prices a call with a cache-read count only (the historical
// signature, kept for callers that do not know cache writes).
func CostBreakdown(model string, input, output, cache int64) (Breakdown, int64, float64, bool) {
	return CostBreakdownFull(model, input, output, cache, 0)
}

// CostBreakdownFull prices a call including cache writes. When a model has no
// explicit cache-write price, writes are billed at the input rate so cached
// traffic is never silently free.
func CostBreakdownFull(model string, input, output, cacheRead, cacheWrite int64) (Breakdown, int64, float64, bool) {
	p, ok := Lookup(model)
	if !ok {
		return Breakdown{}, 0, 0, false
	}
	readRate := p.CacheRead
	if readRate == 0 {
		readRate = p.Input
	}
	writeRate := p.CacheWrite
	if writeRate == 0 {
		writeRate = p.Input
	}
	if cacheRead < 0 {
		cacheRead = 0
	}
	if cacheRead > input {
		cacheRead = input
	}
	if cacheWrite < 0 {
		cacheWrite = 0
	}
	b := Breakdown{
		InputUncached: float64(input-cacheRead) * p.Input,
		CacheRead:     float64(cacheRead) * readRate,
		CacheWrite:    float64(cacheWrite) * writeRate,
		Output:        float64(output) * p.Output,
	}
	b.Total = b.InputUncached + b.CacheRead + b.CacheWrite + b.Output
	pct := 0.0
	if p.Context > 0 {
		pct = float64(input) * 100 / float64(p.Context)
	}
	return b, p.Context, pct, true
}

// Cost is the one-line form used by callers that only need the total.
func Cost(model string, input, output, cache int64) (float64, int64, float64, bool) {
	b, ctx, pct, ok := CostBreakdown(model, input, output, cache)
	return b.Total, ctx, pct, ok
}

// Models returns the number of known models across all sources.
func Models() int {
	mu.RLock()
	defer mu.RUnlock()
	return modelCountLocked()
}

func modelCountLocked() int {
	seen := map[string]bool{}
	for k := range embedded {
		seen[k] = true
	}
	for k := range overrides {
		seen[k] = true
	}
	for k := range fallback {
		seen[k] = true
	}
	return len(seen)
}
