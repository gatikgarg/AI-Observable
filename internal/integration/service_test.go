package integration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ai-observable/ai-observable/internal/config"
	"github.com/ai-observable/ai-observable/internal/control"
	"github.com/ai-observable/ai-observable/internal/store"
	"github.com/ai-observable/ai-observable/internal/vault"
)

type fixture struct {
	svc      *Service
	control  *control.DB
	home     string
	env      map[string]string
	provider control.Provider
}

func newFixture(t *testing.T, kind string) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := control.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	v, err := vault.Open(filepath.Join(dir, "keys"), "file")
	if err != nil {
		t.Fatal(err)
	}
	p, err := db.CreateProvider(ctx, control.Provider{
		Name: kind, Kind: kind, BaseURL: "https://example.test", AuthStyle: "bearer",
		AuthMode: "managed", Enabled: true, Origin: "ui",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.AddCredential(ctx, p.ID, "test", "", "sk-test-provider-key", v); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "metrics"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	cfg := config.Default()
	cfg.Server.ProxyAddr = "127.0.0.1:17878"
	home := filepath.Join(dir, "home")
	if err = os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	f := &fixture{control: db, home: home, env: map[string]string{}, provider: p}
	f.svc = New(db, st, v, cfg, dir)
	f.svc.Home = home
	f.svc.SetEnv = func(name, value string) error {
		f.env[name] = value
		return nil
	}
	f.svc.GetEnv = func(name string) (string, error) {
		v, ok := f.env[name]
		if !ok {
			return "", os.ErrNotExist
		}
		return v, nil
	}
	f.svc.UnsetEnv = func(name string) error {
		delete(f.env, name)
		return nil
	}
	return f
}

func TestCodexConfigurationPreservesCRLFAndRollsBack(t *testing.T) {
	f := newFixture(t, "deepseek")
	path := filepath.Join(f.home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := append([]byte{0xef, 0xbb, 0xbf}, []byte(strings.ReplaceAll(`
# keep this user comment
model = "gpt-4o"
model_provider = "openai"

[model_providers.existing]
name = "Existing"
base_url = "https://example.invalid/v1"
wire_api = "responses"
`, "\n", "\r\n"))...)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	created, err := f.svc.Create(context.Background(), CreateRequest{
		AgentType: AgentCodex, Label: "codex-test", ProviderID: f.provider.ID,
		Model: "deepseek-flash", Capture: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Token, "obs_gw_") || len(created.Plan.FileChange) != 1 {
		t.Fatalf("unexpected create result: token prefix ok=%v files=%d",
			strings.HasPrefix(created.Token, "obs_gw_"), len(created.Plan.FileChange))
	}
	beforeApply, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeApply, original) {
		t.Fatal("plan modified the config")
	}
	applied, err := f.svc.Apply(context.Background(), created.Integration.ID, ApplyRequest{
		PlanID: created.Plan.ID, PlanHash: created.Plan.PlanHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if applied.BackupID == "" || len(applied.EnvChanged) != 1 {
		t.Fatalf("apply result: %+v", applied)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(after, []byte{0xef, 0xbb, 0xbf}) {
		t.Fatal("managed file retained a BOM")
	}
	if !bytes.Contains(after, []byte("\r\n")) || bytes.Contains(bytes.ReplaceAll(after, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatal("managed file did not preserve CRLF")
	}
	text := string(after)
	if !strings.Contains(text, "# keep this user comment") || !strings.Contains(text, "[model_providers.existing]") {
		t.Fatal("unrelated user config was lost")
	}
	if !strings.Contains(text, `wire_api = "responses"`) || !strings.Contains(text, "# >>> observable:provider id=") {
		t.Fatal("managed provider block missing")
	}
	if strings.Contains(text, created.Token) {
		t.Fatal("gateway token was written to the config")
	}
	for name, value := range f.env {
		if value != created.Token {
			t.Fatalf("env %s did not receive the token", name)
		}
	}

	again, err := f.svc.Apply(context.Background(), created.Integration.ID, ApplyRequest{
		PlanID: created.Plan.ID, PlanHash: created.Plan.PlanHash,
	})
	if err != nil {
		t.Fatalf("idempotent apply: %v", err)
	}
	if again.BackupID != applied.BackupID {
		t.Fatalf("idempotent apply created a second backup")
	}

	rolled, err := f.svc.Rollback(context.Background(), created.Integration.ID)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored, original) || len(rolled.Conflicts) != 0 {
		t.Fatalf("rollback did not restore originals: conflicts=%v", rolled.Conflicts)
	}
	if len(f.env) != 0 {
		t.Fatalf("rollback left environment values: %v", f.env)
	}
}

func TestCodexApplyRejectsChangedConfig(t *testing.T) {
	f := newFixture(t, "openai")
	created, err := f.svc.Create(context.Background(), CreateRequest{
		AgentType: AgentCodex, Label: "codex", ProviderID: f.provider.ID, Model: "gpt-4o-mini",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := created.Plan.FileChange[0].Path
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("changed = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.Apply(context.Background(), created.Integration.ID, ApplyRequest{
		PlanID: created.Plan.ID, PlanHash: created.Plan.PlanHash,
	}); err == nil {
		t.Fatal("stale plan was applied")
	}
}

func TestMalformedCodexConfigIsNotModified(t *testing.T) {
	f := newFixture(t, "openai")
	path := filepath.Join(f.home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	bad := []byte("[broken\n")
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Create(context.Background(), CreateRequest{
		AgentType: AgentCodex, Label: "codex", ProviderID: f.provider.ID, Model: "gpt-4o-mini",
	}); err == nil || !strings.Contains(err.Error(), "not valid TOML") {
		t.Fatalf("create error = %v", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, bad) {
		t.Fatal("malformed config was modified")
	}
}

func TestRollbackConflictPreservesLaterUserEdits(t *testing.T) {
	f := newFixture(t, "openai")
	created, err := f.svc.Create(context.Background(), CreateRequest{
		AgentType: AgentCodex, Label: "conflict", ProviderID: f.provider.ID, Model: "gpt-4o-mini",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.Apply(context.Background(), created.Integration.ID, ApplyRequest{
		PlanID: created.Plan.ID, PlanHash: created.Plan.PlanHash,
	}); err != nil {
		t.Fatal(err)
	}
	path := created.Plan.FileChange[0].Path
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fh.WriteString("# later user edit\n")
	fh.Close()
	edited, _ := os.ReadFile(path)
	out, err := f.svc.Rollback(context.Background(), created.Integration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Conflicts) != 1 {
		t.Fatalf("conflicts=%v, want one", out.Conflicts)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, edited) {
		t.Fatal("rollback overwrote a later user edit")
	}
}

func TestTwoCodexIntegrationsKeepManagedProvidersValid(t *testing.T) {
	f := newFixture(t, "openai")
	first, err := f.svc.Create(context.Background(), CreateRequest{
		AgentType: AgentCodex, Label: "first", ProviderID: f.provider.ID, Model: "gpt-4o-mini",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.Apply(context.Background(), first.Integration.ID, ApplyRequest{
		PlanID: first.Plan.ID, PlanHash: first.Plan.PlanHash,
	}); err != nil {
		t.Fatal(err)
	}
	second, err := f.svc.Create(context.Background(), CreateRequest{
		AgentType: AgentCodex, Label: "second", ProviderID: f.provider.ID, Model: "gpt-4o",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.Apply(context.Background(), second.Integration.ID, ApplyRequest{
		PlanID: second.Plan.ID, PlanHash: second.Plan.PlanHash,
	}); err != nil {
		t.Fatal(err)
	}
	path := second.Plan.FileChange[0].Path
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if strings.Count(text, "# >>> observable:top id=") != 1 ||
		strings.Count(text, "# >>> observable:provider id=") != 2 {
		t.Fatalf("managed block accounting is wrong:\n%s", text)
	}
	if !strings.Contains(text, "model = \"gpt-4o\"") ||
		!strings.Contains(text, second.Integration.ID) {
		t.Fatal("second integration is not the active Codex provider")
	}
}

func TestGuidedAgentAndTokenLifecycle(t *testing.T) {
	f := newFixture(t, "deepseek")
	if _, err := f.control.CreateProvider(context.Background(), control.Provider{
		Name: "placeholder", Kind: "openai", BaseURL: "https://placeholder.invalid",
		AuthStyle: "bearer", AuthMode: "client", Enabled: true, Origin: "config",
	}); err != nil {
		t.Fatal(err)
	}
	providers, err := f.svc.CompatibleProviders(context.Background(), AgentCline)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range providers {
		if p.Name == "placeholder" {
			t.Fatal("config-imported placeholder appeared in API Manager compatibility")
		}
	}
	created, err := f.svc.Create(context.Background(), CreateRequest{
		AgentType: AgentCline, Label: "cline-main", ProviderID: f.provider.ID, Model: "deepseek-flash",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Plan.Manual) == 0 || len(created.Plan.FileChange) != 0 {
		t.Fatal("Cline must use guided setup")
	}
	id := created.Integration.ID
	token := created.Token
	binding, err := f.svc.Authenticate(context.Background(), token)
	if err != nil || binding.IntegrationID != id {
		t.Fatalf("authenticate: %+v %v", binding, err)
	}
	rotated, err := f.svc.Rotate(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rotated == token {
		t.Fatal("rotation returned the same token")
	}
	if _, err = f.svc.Authenticate(context.Background(), token); err == nil {
		t.Fatal("old token still authenticates")
	}
	if _, err = f.svc.Authenticate(context.Background(), rotated); err != nil {
		t.Fatalf("new token: %v", err)
	}
	if _, err = f.svc.Rollback(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.Authenticate(context.Background(), rotated); err == nil {
		t.Fatal("rolled-back integration still authenticates")
	}
	if _, err = f.svc.Disconnect(context.Background(), id, false); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.Authenticate(context.Background(), rotated); err == nil {
		t.Fatal("revoked token authenticates")
	}
}
