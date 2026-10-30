package provider

import "sort"

// Preset drives the "add provider" grid so no URL is ever hard-coded in JS.
// DocsURL is rendered as a link; nothing fetches it.
type Preset struct {
	Kind            string       `json:"kind"`
	Label           string       `json:"label"`
	DefaultBaseURL  string       `json:"default_base_url"`
	PathPrefix      string       `json:"path_prefix"`
	DocsURL         string       `json:"docs_url"`
	AuthStyles      []string     `json:"auth_styles"`
	AuthModes       []string     `json:"auth_modes"`
	Capabilities    Capabilities `json:"capabilities"`
	NeedsDeployment bool         `json:"needs_deployment"`
	Note            string       `json:"note"`
}

var (
	authStylesAll = []string{"bearer", "x-api-key", "api-key", "x-goog-api-key", "query-key", "none"}
	authModesAll  = []string{"managed", "client"}
)

// presets is ordered for display; the sort in Presets() keeps the order stable
// regardless of map iteration.
var presets = []Preset{
	{
		Kind:           "openai",
		Label:          "OpenAI",
		DefaultBaseURL: "https://api.openai.com",
		PathPrefix:     "/v1",
		DocsURL:        "https://platform.openai.com/docs/api-reference",
		AuthStyles:     []string{"bearer"},
		AuthModes:      authModesAll,
		Capabilities:   Capabilities{Models: true, Usage: true, Costs: true},
		Note:           "Usage and Costs need an Admin API key (sk-admin-...).",
	},
	{
		Kind:           "anthropic",
		Label:          "Anthropic",
		DefaultBaseURL: "https://api.anthropic.com",
		PathPrefix:     "/v1",
		DocsURL:        "https://docs.anthropic.com/en/api",
		AuthStyles:     []string{"x-api-key"},
		AuthModes:      authModesAll,
		Capabilities:   Capabilities{Models: true, Usage: true, Costs: true},
		Note:           "Usage and Costs need an Admin API key (sk-ant-admin...).",
	},
	{
		Kind:            "azure-openai",
		Label:           "Azure OpenAI",
		DefaultBaseURL:  "https://<resource>.openai.azure.com/openai",
		PathPrefix:      "",
		DocsURL:         "https://learn.microsoft.com/azure/ai-services/openai/",
		AuthStyles:      []string{"api-key"},
		AuthModes:       authModesAll,
		Capabilities:    Capabilities{},
		NeedsDeployment: true,
		Note:            "Deployment and api-version are extra fields. Usage history is not exposed by Azure OpenAI.",
	},
	{
		Kind:           "gemini",
		Label:          "Google Gemini",
		DefaultBaseURL: "https://generativelanguage.googleapis.com",
		PathPrefix:     "/v1beta",
		DocsURL:        "https://ai.google.dev/api",
		AuthStyles:     []string{"x-goog-api-key", "query-key"},
		AuthModes:      authModesAll,
		Capabilities:   Capabilities{Models: true, NativeUsageInResponse: true},
		Note:           "Responses carry usageMetadata. Per-key usage history needs Google Cloud billing.",
	},
	{
		Kind:           "deepseek",
		Label:          "DeepSeek",
		DefaultBaseURL: "https://api.deepseek.com",
		PathPrefix:     "/v1",
		DocsURL:        "https://api-docs.deepseek.com",
		AuthStyles:     []string{"bearer"},
		AuthModes:      authModesAll,
		Capabilities:   Capabilities{Models: true, Balance: true},
		Note:           "No usage history API; balance is available at /user/balance.",
	},
	{
		Kind:           "openrouter",
		Label:          "OpenRouter",
		DefaultBaseURL: "https://openrouter.ai/api",
		PathPrefix:     "/v1",
		DocsURL:        "https://openrouter.ai/docs",
		AuthStyles:     []string{"bearer"},
		AuthModes:      authModesAll,
		Capabilities:   Capabilities{Models: true, Usage: true, Costs: true, Balance: true, NativeUsageInResponse: true},
		Note:           "Responses carry usage.cost; /credits reports prepaid credit.",
	},
	{
		Kind:           "groq",
		Label:          "Groq",
		DefaultBaseURL: "https://api.groq.com/openai",
		PathPrefix:     "/v1",
		DocsURL:        "https://console.groq.com/docs",
		AuthStyles:     []string{"bearer"},
		AuthModes:      authModesAll,
		Capabilities:   Capabilities{Models: true},
		Note:           "Models only: no usage or cost history API.",
	},
	{
		Kind:           "local",
		Label:          "Local (Ollama / LM Studio / vLLM)",
		DefaultBaseURL: "http://127.0.0.1:11434/v1",
		PathPrefix:     "",
		DocsURL:        "https://github.com/ollama/ollama/blob/main/docs/openai.md",
		AuthStyles:     []string{"none", "bearer"},
		AuthModes:      authModesAll,
		Capabilities:   Capabilities{Models: true},
		Note:           "Cost is 0 by definition; nothing to bill or reconcile.",
	},
	{
		Kind:           "openai-compatible",
		Label:          "OpenAI-compatible gateway",
		DefaultBaseURL: "",
		PathPrefix:     "/v1",
		DocsURL:        "",
		AuthStyles:     authStylesAll,
		AuthModes:      authModesAll,
		Capabilities:   Capabilities{Models: true},
		Note:           "Together, Fireworks, vLLM or any gateway that speaks the OpenAI schema. Base URL is required.",
	},
}

// Presets returns the grid in display order.
func Presets() []Preset {
	out := make([]Preset, len(presets))
	copy(out, presets)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// PresetFor returns the preset for kind.
func PresetFor(kind string) (Preset, bool) {
	for _, p := range presets {
		if p.Kind == kind {
			return p, true
		}
	}
	return Preset{}, false
}

// ValidAuthStyle reports whether style is one of the six wire styles.
func ValidAuthStyle(style string) bool {
	for _, s := range authStylesAll {
		if s == style {
			return true
		}
	}
	return false
}

// ValidAuthMode reports whether mode is managed or client.
func ValidAuthMode(mode string) bool {
	return mode == "managed" || mode == "client"
}
