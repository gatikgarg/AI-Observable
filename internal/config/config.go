// Package config owns the on-disk llmtap configuration (config.toml).
//
// Every key that existed before the API Manager work keeps its name, type and
// default. New sections are additive and fall back to sane defaults when the
// file predates them.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Config is the parsed config.toml.
type Config struct {
	Server    Server              `toml:"server"`
	Admin     Admin               `toml:"admin"`
	Capture   Capture             `toml:"capture"`
	Snapshot  Snapshot            `toml:"snapshot"`
	Collector Collector           `toml:"collector"`
	Vault     Vault               `toml:"vault"`
	Pricing   Pricing             `toml:"pricing"`
	Providers map[string]Provider `toml:"providers"`
}

// Server holds the two listener addresses. Ports are always part of the
// address; the CLI flags only replace the port and preserve the host.
type Server struct {
	ProxyAddr   string `toml:"proxy_addr"`
	AdminAddr   string `toml:"admin_addr"`
	GrafanaURL  string `toml:"grafana_url"`
	OpenOnStart bool   `toml:"open_on_start"`
}

// Admin controls the admin listener's token requirement.
type Admin struct {
	Token                   string `toml:"token"`
	RequireTokenOffLoopback bool   `toml:"require_token_off_loopback"`
}

// Capture controls per-call preview capture.
type Capture struct {
	Bodies        string `toml:"bodies"`
	MaxBodyBytes  int    `toml:"max_body_bytes"`
	RetentionDays int    `toml:"retention_days"`
}

// Snapshot controls how often the read-only view DB is refreshed.
type Snapshot struct {
	IntervalSeconds int `toml:"interval_seconds"`
}

// Collector controls the lane B/C pull scheduler.
type Collector struct {
	Enabled                bool    `toml:"enabled"`
	UsageIntervalSeconds   int     `toml:"usage_interval_seconds"`
	CostsIntervalSeconds   int     `toml:"costs_interval_seconds"`
	ModelsIntervalSeconds  int     `toml:"models_interval_seconds"`
	BalanceIntervalSeconds int     `toml:"balance_interval_seconds"`
	BackfillDays           int     `toml:"backfill_days"`
	ProbeBudgetUSD         float64 `toml:"probe_budget_usd"`
	MaxConcurrent          int     `toml:"max_concurrent"`
	LaneD                  bool    `toml:"lane_d"`
}

// Vault controls credential key resolution and reveal policy.
type Vault struct {
	Mode        string `toml:"mode"`
	AllowReveal bool   `toml:"allow_reveal"`
}

// Pricing controls the optional user price file.
type Pricing struct {
	File         string `toml:"file"`
	RefreshHours int    `toml:"refresh_hours"`
}

// Provider is one pre-existing [providers.*] entry. It is imported into the
// control DB as read-only (origin=config) and never rewritten.
type Provider struct {
	Upstream    string `toml:"upstream"`
	StreamUsage bool   `toml:"stream_usage"`
}

// ProviderName is the proxy path segment allowed for a provider.
var ProviderName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,31}$`)

// Path returns <dir>/config.toml.
func Path(dir string) string { return filepath.Join(dir, "config.toml") }

// Default returns the configuration written on first run.
func Default() Config {
	return Config{
		Server: Server{
			ProxyAddr:   "127.0.0.1:7878",
			AdminAddr:   "127.0.0.1:7879",
			GrafanaURL:  "http://127.0.0.1:3000",
			OpenOnStart: false,
		},
		Admin: Admin{
			Token:                   "",
			RequireTokenOffLoopback: true,
		},
		Capture: Capture{
			Bodies:        "truncated",
			MaxBodyBytes:  4096,
			RetentionDays: 90,
		},
		Snapshot: Snapshot{IntervalSeconds: 5},
		Collector: Collector{
			Enabled:                true,
			UsageIntervalSeconds:   900,
			CostsIntervalSeconds:   3600,
			ModelsIntervalSeconds:  21600,
			BalanceIntervalSeconds: 1800,
			BackfillDays:           7,
			ProbeBudgetUSD:         0,
			MaxConcurrent:          3,
			LaneD:                  false,
		},
		Vault:   Vault{Mode: "file", AllowReveal: false},
		Pricing: Pricing{File: "", RefreshHours: 24},
		Providers: map[string]Provider{
			"openai":     {Upstream: "https://api.openai.com", StreamUsage: true},
			"deepseek":   {Upstream: "https://api.deepseek.com", StreamUsage: true},
			"openrouter": {Upstream: "https://openrouter.ai/api", StreamUsage: true},
			"groq":       {Upstream: "https://api.groq.com/openai", StreamUsage: true},
		},
	}
}

// Load reads config.toml, writing a default file (mode 0600) when it is
// missing. Unknown keys are ignored so newer files keep working.
func Load(dir string) (Config, error) {
	c := Default()
	p := Path(dir)
	if _, e := os.Stat(p); os.IsNotExist(e) {
		if e = os.MkdirAll(dir, 0755); e != nil {
			return c, e
		}
		b, _ := toml.Marshal(c)
		return c, os.WriteFile(p, b, 0600)
	}
	b, e := os.ReadFile(p)
	if e != nil {
		return c, e
	}
	if e = toml.Unmarshal(b, &c); e != nil {
		return c, e
	}
	// A trailing slash in grafana_url would turn every dashboard link into
	// "//d/<uid>", which Grafana answers with 404, so normalise it here once.
	c.Server.GrafanaURL = strings.TrimRight(strings.TrimSpace(c.Server.GrafanaURL), "/")
	return c, Validate(c)
}

// Validate rejects unusable values. Invalid values fail the load instead of
// silently degrading: a wrong bind address must never look like a healthy
// process.
func Validate(c Config) error {
	for n, p := range c.Providers {
		if !ProviderName.MatchString(n) {
			return fmt.Errorf("provider %q is not a valid name (%s)", n, ProviderName)
		}
		u, e := url.Parse(p.Upstream)
		if e != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("provider %s has invalid upstream %q", n, p.Upstream)
		}
	}
	if c.Capture.Bodies != "none" && c.Capture.Bodies != "truncated" && c.Capture.Bodies != "full" {
		return fmt.Errorf("capture.bodies must be none, truncated, or full")
	}
	if c.Capture.RetentionDays < 0 {
		return fmt.Errorf("capture.retention_days must be >= 0")
	}
	if c.Snapshot.IntervalSeconds < 1 {
		return fmt.Errorf("snapshot.interval_seconds must be >= 1")
	}
	if c.Collector.ProbeBudgetUSD < 0 {
		return fmt.Errorf("collector.probe_budget_usd must be >= 0")
	}
	for name, v := range map[string]int{
		"collector.usage_interval_seconds":   c.Collector.UsageIntervalSeconds,
		"collector.costs_interval_seconds":   c.Collector.CostsIntervalSeconds,
		"collector.models_interval_seconds":  c.Collector.ModelsIntervalSeconds,
		"collector.balance_interval_seconds": c.Collector.BalanceIntervalSeconds,
	} {
		if v < 300 {
			return fmt.Errorf("%s must be >= 300", name)
		}
	}
	if c.Collector.BackfillDays < 0 {
		return fmt.Errorf("collector.backfill_days must be >= 0")
	}
	if c.Collector.MaxConcurrent < 1 {
		return fmt.Errorf("collector.max_concurrent must be >= 1")
	}
	switch c.Vault.Mode {
	case "", "file", "env", "disabled":
	default:
		return fmt.Errorf("vault.mode must be file, env, or disabled")
	}
	return nil
}

// IsLoopback reports whether addr binds a loopback interface.
func IsLoopback(addr string) bool {
	host, _, e := net.SplitHostPort(addr)
	if e != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// WithPort replaces the port of an existing host:port address, keeping the
// host. This is what --proxy-port / --admin-port mean (R16).
func WithPort(addr, port string) string {
	host, _, e := net.SplitHostPort(addr)
	if e != nil {
		host = addr
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), strings.TrimSpace(port))
}
