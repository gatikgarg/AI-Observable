package integration

import (
	"context"

	"github.com/ai-observable/ai-observable/internal/control"
)

const (
	AgentCodex    = "codex"
	AgentCline    = "cline"
	AgentKiloCode = "kilo-code"
)

type Detection struct {
	Installed   bool     `json:"installed"`
	Version     string   `json:"version,omitempty"`
	ConfigPaths []string `json:"config_paths,omitempty"`
	Notes       []string `json:"notes,omitempty"`
}

type AgentView struct {
	ID            string    `json:"id"`
	DisplayName   string    `json:"display_name"`
	RequiredWire  string    `json:"required_wire"`
	SetupMode     string    `json:"setup_mode"`
	Support       string    `json:"support"`
	SupportReason string    `json:"support_reason,omitempty"`
	Detection     Detection `json:"detection"`
}

type ProviderView struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Enabled      bool     `json:"enabled"`
	Chat         bool     `json:"chat_completions"`
	Responses    bool     `json:"responses"`
	Models       bool     `json:"models_list"`
	Compatible   bool     `json:"compatible"`
	Reason       string   `json:"reason,omitempty"`
	ModelsList   []string `json:"models"`
	ModelsSource string   `json:"models_source"`
}

type Step struct {
	Title      string   `json:"title"`
	Text       string   `json:"text"`
	CopyValues []string `json:"copy_values,omitempty"`
}

type FileChange struct {
	Path              string `json:"path"`
	Exists            bool   `json:"exists"`
	SHA256Before      string `json:"sha256_before"`
	SHA256After       string `json:"sha256_after"`
	NewContentPreview string `json:"new_content_preview"`
	RedactedDiff      string `json:"redacted_diff"`
	LineEnding        string `json:"line_ending"`
	Permissions       string `json:"permissions"`
}

type EnvChange struct {
	Name   string `json:"name"`
	Scope  string `json:"scope"`
	Action string `json:"action"`
}

type Restart struct {
	Required bool     `json:"required"`
	What     []string `json:"what,omitempty"`
}

type Plan struct {
	ID         string       `json:"plan_id"`
	PlanHash   string       `json:"plan_hash"`
	FileChange []FileChange `json:"file_changes,omitempty"`
	EnvChanges []EnvChange  `json:"env_changes,omitempty"`
	Manual     []Step       `json:"manual_steps,omitempty"`
	Restart    Restart      `json:"restart"`
	Warnings   []string     `json:"warnings,omitempty"`
	BackupDir  string       `json:"backup_location,omitempty"`
}

type CreateRequest struct {
	AgentType  string `json:"agent_type"`
	Label      string `json:"label"`
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
	Capture    string `json:"capture_mode"`
}

type CreateResult struct {
	Integration control.Integration `json:"integration"`
	Token       string              `json:"token"`
	Plan        Plan                `json:"plan"`
}

type ApplyRequest struct {
	PlanID   string `json:"plan_id"`
	PlanHash string `json:"plan_hash"`
}

type ApplyResult struct {
	BackupID     string   `json:"backup_id,omitempty"`
	ChangedFiles []string `json:"changed_files,omitempty"`
	EnvChanged   []string `json:"env_changed,omitempty"`
	State        string   `json:"state"`
	Restart      Restart  `json:"restart"`
}

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type VerifyResult struct {
	State  string  `json:"state"`
	Checks []Check `json:"checks"`
}

type RollbackResult struct {
	Restored  []string `json:"restored,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
	State     string   `json:"state"`
}

type Binding struct {
	IntegrationID  string
	AgentType      string
	Label          string
	ProviderID     string
	ProviderName   string
	ProviderKind   string
	Model          string
	CaptureMode    string
	GatewayBaseURL string
}

type fileDraft struct {
	Path         string
	Original     []byte
	Exists       bool
	Content      []byte
	LineEnding   string
	Permissions  string
	SHA256Before string
	SHA256After  string
	Preview      string
	Diff         string
}

type planDraft struct {
	Files     []fileDraft
	Env       []EnvChange
	Manual    []Step
	Restart   Restart
	Warnings  []string
	EnvValues map[string]string
}

type adapter interface {
	ID() string
	DisplayName() string
	RequiredWire() string
	SetupMode() string
	Detect(context.Context) Detection
	Plan(context.Context, Binding) (planDraft, error)
	VerifyConfig(Binding) Check
}

type internalPlan struct {
	Plan      Plan
	Binding   Binding
	Draft     planDraft
	AdapterID string
}
