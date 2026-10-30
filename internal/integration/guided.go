package integration

import (
	"context"
	"path/filepath"
	"strings"
)

type guidedAdapter struct {
	id, name string
	home     string
}

func (a guidedAdapter) ID() string           { return a.id }
func (a guidedAdapter) DisplayName() string  { return a.name }
func (a guidedAdapter) RequiredWire() string { return "chat" }
func (a guidedAdapter) SetupMode() string    { return "guided" }

func (a guidedAdapter) Detect(context.Context) Detection {
	d := Detection{}
	dirs, _ := filepath.Glob(filepath.Join(a.home, ".vscode", "extensions", "*"))
	for _, dir := range dirs {
		base := strings.ToLower(filepath.Base(dir))
		if (a.id == AgentCline && strings.Contains(base, "cline")) ||
			(a.id == AgentKiloCode && strings.Contains(base, "kilo")) {
			d.Installed = true
			d.ConfigPaths = append(d.ConfigPaths, dir)
		}
	}
	if !d.Installed {
		d.Notes = append(d.Notes, "Extension not detected; guided setup is still available.")
	}
	return d
}

func (a guidedAdapter) Plan(_ context.Context, b Binding) (planDraft, error) {
	model := b.Model
	if model == "" {
		model = "<choose or type the provider's model ID>"
	}
	raw := strings.Contains(strings.ToLower(a.name), "kilo")
	keyInstruction := "API Key = the gateway token shown once"
	if raw {
		keyInstruction = "API Key = the gateway token exactly, without a Bearer prefix"
	}
	return planDraft{
		Manual: []Step{
			{Title: "Open agent settings", Text: "Open " + a.name + " settings and choose an OpenAI-compatible provider."},
			{Title: "Set Base URL", Text: "Enter the full gateway API root. Do not add /chat/completions.", CopyValues: []string{b.GatewayBaseURL}},
			{Title: "Set API key", Text: keyInstruction + ". It is displayed only once in this creation response."},
			{Title: "Set Model ID", Text: "Use the exact model ID configured in Observable.", CopyValues: []string{model}},
			{Title: "Reload", Text: "Save, then reload the VS Code window if the provider list does not refresh."},
		},
		Restart: Restart{Required: true, What: []string{"Reload the VS Code window after saving."}},
		Warnings: []string{
			"Observable must be running whenever the agent is used.",
			"Base URL must remain " + b.GatewayBaseURL + "; an extra /v1 or a missing prefix produces a 404.",
			"Supported status remains Not verified yet until real inference traffic is recorded.",
		},
	}, nil
}

func (a guidedAdapter) VerifyConfig(Binding) Check {
	return Check{Name: "guided_config", Status: "skipped", Detail: "agent settings are user-managed"}
}
