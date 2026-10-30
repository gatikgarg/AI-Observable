package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	files := []string{"cost.json", "reliability.json", "latency.json", "efficiency.json"}
	for _, f := range files {
		b, e := os.ReadFile(filepath.Join("deploy/grafana/dashboards", f))
		if e != nil {
			panic(e)
		}
		var d map[string]any
		if e = json.Unmarshal(b, &d); e != nil {
			panic(e)
		}
		if d["uid"] == nil || d["refresh"] != "10s" || d["editable"] != false {
			panic("dashboard metadata invalid: " + f)
		}
		if d["uid"] == "llmtap-cost" || d["uid"] == "llmtap-reliability" || d["uid"] == "llmtap-latency" || d["uid"] == "llmtap-efficiency" {
			vars, ok := d["templating"].(map[string]any)
			if !ok {
				panic("model variable missing: " + f)
			}
			list, ok := vars["list"].([]any)
			if !ok || len(list) == 0 || list[0].(map[string]any)["name"] != "model" {
				panic("model variable invalid: " + f)
			}
			ps, ok := d["panels"].([]any)
			if !ok || len(ps) != 10 {
				panic("addendum dashboard must have 10 panels: " + f)
			}
			for _, raw := range ps {
				p := raw.(map[string]any)
				if p["title"] == nil || p["description"] == nil || p["noValue"] == nil {
					panic("panel metadata missing: " + f)
				}
				ts, ok := p["targets"].([]any)
				if !ok {
					panic("panel targets missing: " + f)
				}
				for _, tr := range ts {
					q := tr.(map[string]any)["query"].(string)
					if !strings.Contains(q, "$__from") || !strings.Contains(q, "$__to") {
						panic("target missing time filter: " + f)
					}
					if tr.(map[string]any)["rawQueryText"] == nil || tr.(map[string]any)["queryText"] == nil || tr.(map[string]any)["timeColumns"] == nil {
						panic("SQLite target fields missing: " + f)
					}
				}
			}
		}
		fmt.Println("valid", f)
	}
}
