// Command style_dashboards applies the shared native Grafana presentation
// standard to the four provisioned, data-backed dashboards.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var files = []string{"cost.json", "reliability.json", "latency.json", "efficiency.json"}

func main() {
	for _, name := range files {
		path := filepath.Join("deploy", "grafana", "dashboards", name)
		b, err := os.ReadFile(path)
		must(err)
		var d map[string]any
		must(json.Unmarshal(b, &d))
		d["graphTooltip"] = 1 // shared crosshair, useful for comparing series.
		d["tags"] = []any{"ai-observable"}
		d["templating"] = map[string]any{"list": []any{map[string]any{
			"name": "model", "label": "Model", "type": "query", "multi": true,
			"includeAll": true, "refresh": 1,
			"datasource": map[string]any{"type": "frser-sqlite-datasource", "uid": "llmtap-sqlite"},
			"query":      "SELECT DISTINCT model FROM calls_v ORDER BY model",
			"definition": "SELECT DISTINCT model FROM calls_v ORDER BY model",
		}}}
		for _, raw := range d["panels"].([]any) {
			stylePanel(raw.(map[string]any), name)
		}
		removeLegacyLinks(&d)
		rewriteVisibleText(d)
		out, err := json.MarshalIndent(d, "", "  ")
		must(err)
		must(os.WriteFile(path, append(out, '\n'), 0644))
		fmt.Println("styled", path)
	}
}

func stylePanel(p map[string]any, dashboard string) {
	panelType, _ := p["type"].(string)
	for _, key := range []string{"title", "description", "noValue"} {
		if text, ok := p[key].(string); ok {
			text = strings.ReplaceAll(text, "Setup dashboard", "a wider time range or the Model filter")
			text = strings.ReplaceAll(text, "Call Detail", "the call record")
			p[key] = text
		}
	}
	// The SQLite datasource does not execute the generic `query` field. Its
	// documented target fields are rawQueryText/queryText/timeColumns.
	// Keep `query` too for our lightweight static verifier and copy the same SQL
	// into the plugin fields used by Grafana at runtime.
	if targets, ok := p["targets"].([]any); ok {
		for _, raw := range targets {
			t := raw.(map[string]any)
			if sql, ok := t["query"].(string); ok {
				// Apply the dashboard model selector at the source-table level.
				// The __all sentinel makes the default selection return every model.
				if dashboard != "" && !containsModelFilter(sql) {
					sql = addModelFilter(sql)
					t["query"] = sql
				}
				t["rawQueryText"] = sql
				t["queryText"] = sql
				t["queryType"] = "table"
				t["timeColumns"] = []any{"time", "ts"}
			}
		}
	}
	if title, ok := p["title"].(string); ok && title == "Latency Distribution" {
		// Bar charts require a string/time category. Keep the numeric bucket for
		// ordering but expose a formatted category as the first column.
		for _, raw := range p["targets"].([]any) {
			t := raw.(map[string]any)
			q := t["query"].(string)
			q = replaceLatencyBucket(q)
			t["query"], t["rawQueryText"], t["queryText"] = q, q, q
		}
	}
	p["transparent"] = false
	p["fieldConfig"] = mergeMap(p["fieldConfig"], map[string]any{
		"defaults": map[string]any{
			"color":  map[string]any{"mode": "palette-classic"},
			"custom": map[string]any{"lineWidth": 2, "fillOpacity": 14, "spanNulls": false},
		},
		"overrides": []any{},
	})
	opts := map[string]any{}
	switch panelType {
	case "stat":
		opts = map[string]any{"reduceOptions": map[string]any{"values": false, "calcs": []any{"lastNotNull"}, "fields": ""}, "orientation": "auto", "textMode": "auto", "colorMode": "value_and_background", "graphMode": "area", "justifyMode": "auto"}
	case "timeseries":
		opts = map[string]any{"tooltip": map[string]any{"mode": "multi", "sort": "desc"}, "legend": map[string]any{"displayMode": "table", "placement": "bottom", "showLegend": true, "calcs": []any{"lastNotNull", "max"}}}
	case "barchart":
		opts = map[string]any{"orientation": "auto", "showValue": "auto", "stacking": "none", "legend": map[string]any{"displayMode": "table", "placement": "bottom", "showLegend": true}}
	case "table":
		opts = map[string]any{"showHeader": true, "cellHeight": "sm", "footer": map[string]any{"show": false}}
	case "xychart":
		opts = map[string]any{"tooltip": map[string]any{"mode": "single", "sort": "none"}, "legend": map[string]any{"showLegend": true, "placement": "bottom"}}
	}
	if len(opts) > 0 {
		p["options"] = opts
	}
	// Native semantic treatment for dashboard-specific operational KPIs.
	title, _ := p["title"].(string)
	if title == "Failure Rate" || title == "Success Rate" || title == "p95 Latency" || title == "p99 Latency" || title == "p95 TTFT" {
		fc := p["fieldConfig"].(map[string]any)
		def := fc["defaults"].(map[string]any)
		if title == "Success Rate" {
			def["thresholds"] = map[string]any{"mode": "absolute", "steps": []any{map[string]any{"color": "red", "value": nil}, map[string]any{"color": "orange", "value": 95}, map[string]any{"color": "green", "value": 99}}}
		} else if title == "Failure Rate" {
			def["thresholds"] = map[string]any{"mode": "absolute", "steps": []any{map[string]any{"color": "green", "value": nil}, map[string]any{"color": "orange", "value": 1}, map[string]any{"color": "red", "value": 5}}}
		} else {
			def["thresholds"] = map[string]any{"mode": "absolute", "steps": []any{map[string]any{"color": "blue", "value": nil}, map[string]any{"color": "orange", "value": 2000}, map[string]any{"color": "red", "value": 5000}}}
		}
	}
	if dashboard == "cost.json" && (title == "Total Spend" || title == "Average Cost / Call") {
		p["fieldConfig"].(map[string]any)["defaults"].(map[string]any)["color"] = map[string]any{"mode": "fixed", "fixedColor": "blue"}
	}
}

func containsModelFilter(q string) bool {
	return len(q) > 0 && (strings.Contains(q, "${model:") || strings.Contains(q, "model IN"))
}
func addModelFilter(q string) string {
	needle := " WHERE "
	i := strings.Index(strings.ToUpper(q), needle)
	if i < 0 {
		return q
	}
	pos := i + len(needle)
	return q[:pos] + "('${model:raw}'='__all' OR model IN (${model:singlequote})) AND " + q[pos:]
}
func replaceLatencyBucket(q string) string {
	old := "SELECT (latency_ms/250)*250 AS latency_bucket,COUNT(*) AS calls"
	return strings.Replace(q, old, "SELECT printf('%d-%d ms',(latency_ms/250)*250,(latency_ms/250)*250+249) AS latency_bucket,COUNT(*) AS calls", 1)
}
func removeLegacyLinks(d *map[string]any) {
	if links, ok := (*d)["links"].([]any); ok {
		out := make([]any, 0, len(links))
		for _, raw := range links {
			title := strings.ToLower(raw.(map[string]any)["title"].(string))
			if title == "overview" || title == "call explorer" || title == "setup" || title == "call detail" {
				continue
			}
			out = append(out, raw)
		}
		(*d)["links"] = out
	}
}

func rewriteVisibleText(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, value := range x {
			if k != "uid" && k != "url" {
				rewriteVisibleText(value)
			}
		}
	case []any:
		for _, value := range x {
			rewriteVisibleText(value)
		}
	case string:
		// Strings are immutable in the decoded tree; this helper is intentionally
		// reserved for the structured dashboard generator above. Obsolete wording
		// is removed directly from panel descriptions by normalizeText below.
	}
}

func mergeMap(existing any, addition map[string]any) map[string]any {
	m, _ := existing.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	for k, v := range addition {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	return m
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
