package integration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ai-observable/ai-observable/internal/config"
	"github.com/ai-observable/ai-observable/internal/control"
	"github.com/ai-observable/ai-observable/internal/gateway"
	"github.com/ai-observable/ai-observable/internal/store"
	"github.com/ai-observable/ai-observable/internal/vault"
)

var ErrUnsupported = errors.New("integration: unsupported agent/provider combination")

type Service struct {
	DB      *control.DB
	Store   *store.Store
	Vault   *vault.Vault
	Config  config.Config
	DataDir string
	Home    string
	Client  *http.Client

	SetEnv   func(string, string) error
	GetEnv   func(string) (string, error)
	UnsetEnv func(string) error

	mu      sync.Mutex
	plans   map[string]*internalPlan
	secrets map[string]string
}

func New(db *control.DB, st *store.Store, v *vault.Vault, cfg config.Config, dataDir string) *Service {
	home, _ := os.UserHomeDir()
	s := &Service{
		DB: db, Store: st, Vault: v, Config: cfg, DataDir: dataDir, Home: home,
		Client: &http.Client{Timeout: 15 * time.Second},
		SetEnv: setUserEnv, GetEnv: getUserEnv, UnsetEnv: unsetUserEnv,
		plans: map[string]*internalPlan{}, secrets: map[string]string{},
	}
	return s
}

func (s *Service) adapters() []adapter {
	return []adapter{
		&codexAdapter{Home: s.Home},
		guidedAdapter{id: AgentCline, name: "Cline", home: s.Home},
		guidedAdapter{id: AgentKiloCode, name: "Kilo Code", home: s.Home},
	}
}

func (s *Service) adapter(id string) (adapter, bool) {
	for _, a := range s.adapters() {
		if a.ID() == id {
			return a, true
		}
	}
	return nil, false
}

func (s *Service) Agents(ctx context.Context) []AgentView {
	out := []AgentView{}
	for _, a := range s.adapters() {
		out = append(out, AgentView{
			ID: a.ID(), DisplayName: a.DisplayName(), RequiredWire: a.RequiredWire(),
			SetupMode: a.SetupMode(), Support: "Not verified yet", Detection: a.Detect(ctx),
		})
	}
	return out
}

func (s *Service) CompatibleProviders(ctx context.Context, agent string) ([]ProviderView, error) {
	a, ok := s.adapter(agent)
	if !ok {
		return nil, control.ErrNotFound
	}
	providers, err := s.DB.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ProviderView, 0, len(providers))
	for _, p := range providers {
		// Providers imported from config.toml are kept for legacy proxy routes,
		// but they are not API Manager rows and have no Observable credential.
		if p.Origin == "config" {
			continue
		}
		chat, responses := providerWire(p.Kind)
		compatible := a.RequiredWire() == "responses" && responses ||
			a.RequiredWire() == "chat" && chat
		reason := ""
		if !compatible {
			reason = "This agent requires " + wireLabel(a.RequiredWire()) + "; provider " + p.Name + " does not advertise it."
		}
		if !p.Enabled {
			reason = "Provider is disabled."
			compatible = false
		}
		models, source := s.providerModels(ctx, p.Name)
		out = append(out, ProviderView{
			ID: p.ID, Name: p.Name, Kind: p.Kind, Enabled: p.Enabled, Chat: chat, Responses: responses,
			Models: true, Compatible: compatible, Reason: reason, ModelsList: models, ModelsSource: source,
		})
	}
	return out, nil
}

func providerWire(kind string) (chat, responses bool) {
	switch kind {
	case "openai":
		return true, true
	case "deepseek":
		// Verified in current official DeepSeek docs on 2026-10-28.
		return true, true
	case "openrouter", "groq", "local", "openai-compatible":
		return true, false
	default:
		return false, false
	}
}

func wireLabel(w string) string {
	if w == "responses" {
		return "the Responses API"
	}
	return "Chat Completions"
}

func (s *Service) providerModels(ctx context.Context, provider string) ([]string, string) {
	if s.Store == nil {
		return []string{}, "manual"
	}
	rows, err := s.Store.DB.QueryContext(ctx,
		`SELECT model_id FROM model_catalog WHERE provider=? ORDER BY model_id LIMIT 200`, provider)
	if err != nil {
		return []string{}, "manual"
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var model string
		if rows.Scan(&model) == nil {
			out = append(out, model)
		}
	}
	if len(out) > 0 {
		return out, "provider catalog"
	}
	return out, "manual"
}

func (s *Service) Create(ctx context.Context, req CreateRequest) (CreateResult, error) {
	a, ok := s.adapter(strings.TrimSpace(req.AgentType))
	if !ok {
		return CreateResult{}, fmt.Errorf("%w: unknown agent type", control.ErrInvalid)
	}
	provider, err := s.DB.GetProvider(ctx, req.ProviderID)
	if err != nil {
		return CreateResult{}, err
	}
	chat, responses := providerWire(provider.Kind)
	if a.RequiredWire() == "responses" && !responses {
		return CreateResult{}, fmt.Errorf("%w: %s requires Responses and %s offers Chat Completions only",
			ErrUnsupported, a.DisplayName(), provider.Name)
	}
	if a.RequiredWire() == "chat" && !chat {
		return CreateResult{}, fmt.Errorf("%w: %s requires Chat Completions", ErrUnsupported, a.DisplayName())
	}
	req.Label = strings.TrimSpace(req.Label)
	if req.Label == "" {
		req.Label = a.ID()
	}
	req.Model = strings.TrimSpace(req.Model)
	if req.Capture == "" {
		req.Capture = "none"
	}
	id := newIntegrationID()
	token, hash, prefix, err := issueToken()
	if err != nil {
		return CreateResult{}, err
	}
	b := Binding{
		IntegrationID: id, AgentType: a.ID(), Label: req.Label, ProviderID: provider.ID,
		ProviderName: provider.Name, ProviderKind: provider.Kind, Model: req.Model,
		CaptureMode: req.Capture, GatewayBaseURL: s.gatewayURL(),
	}
	draft, err := a.Plan(ctx, b)
	if err != nil {
		return CreateResult{}, err
	}
	for name := range draft.EnvValues {
		draft.EnvValues[name] = token
	}
	created, err := s.DB.CreateIntegration(ctx, control.Integration{
		ID: id, AgentType: a.ID(), Label: req.Label, ProviderConfigID: provider.ID,
		Model: req.Model, TokenHash: hash, TokenPrefix: prefix, CaptureMode: req.Capture,
		State: "planned", CreatedAtMS: time.Now().UnixMilli(), PortPinned: true,
		GatewayBaseURL: b.GatewayBaseURL,
	})
	if err != nil {
		return CreateResult{}, err
	}
	plan := s.makePlan(b, draft)
	s.mu.Lock()
	s.plans[plan.ID] = &internalPlan{Plan: plan, Binding: b, Draft: draft, AdapterID: a.ID()}
	s.secrets[id] = token
	s.mu.Unlock()
	return CreateResult{Integration: created, Token: token, Plan: plan}, nil
}

func (s *Service) makePlan(b Binding, d planDraft) Plan {
	plan := Plan{
		ID: newIntegrationID(), Manual: d.Manual, Restart: d.Restart, Warnings: d.Warnings,
		EnvChanges: d.Env,
		BackupDir:  filepath.Join(s.DataDir, "backups", b.IntegrationID),
	}
	for _, f := range d.Files {
		plan.FileChange = append(plan.FileChange, FileChange{
			Path: f.Path, Exists: f.Exists, SHA256Before: f.SHA256Before,
			SHA256After: f.SHA256After, NewContentPreview: f.Preview, RedactedDiff: f.Diff,
			LineEnding: f.LineEnding, Permissions: f.Permissions,
		})
	}
	buf, _ := json.Marshal(struct {
		Integration string
		Files       []FileChange
		Env         []EnvChange
		Restart     Restart
	}{b.IntegrationID, plan.FileChange, plan.EnvChanges, plan.Restart})
	sum := sha256.Sum256(buf)
	plan.PlanHash = hex.EncodeToString(sum[:])
	return plan
}

func (s *Service) Apply(ctx context.Context, id string, req ApplyRequest) (ApplyResult, error) {
	s.mu.Lock()
	ip := s.plans[req.PlanID]
	s.mu.Unlock()
	if ip == nil || ip.Binding.IntegrationID != id {
		return ApplyResult{}, fmt.Errorf("%w: unknown or stale plan", control.ErrConflict)
	}
	if subtle.ConstantTimeCompare([]byte(req.PlanHash), []byte(ip.Plan.PlanHash)) != 1 {
		return ApplyResult{}, fmt.Errorf("%w: plan hash does not match", control.ErrConflict)
	}
	out := ApplyResult{State: "applied", Restart: ip.Plan.Restart}
	if len(ip.Draft.Files) == 0 {
		if err := s.DB.SetIntegrationState(ctx, id, "applied", ""); err != nil {
			return ApplyResult{}, err
		}
		return out, nil
	}

	var err error
	current, err := s.DB.GetIntegration(ctx, id)
	if err != nil {
		return ApplyResult{}, err
	}
	allAfter := true
	for _, f := range ip.Draft.Files {
		var got string
		got, err = hashFile(f.Path)
		if err != nil {
			return ApplyResult{}, err
		}
		if got == f.SHA256After {
			continue
		}
		allAfter = false
		if got != f.SHA256Before {
			return ApplyResult{}, fmt.Errorf("%w: %s changed since planning", control.ErrConflict, f.Path)
		}
	}
	if allAfter {
		for _, e := range ip.Draft.Env {
			value := ip.Draft.EnvValues[e.Name]
			if err = s.SetEnv(e.Name, value); err != nil {
				return ApplyResult{}, err
			}
			out.EnvChanged = append(out.EnvChanged, e.Name)
		}
		out.BackupID = current.AppliedBackupID
		if err = s.DB.SetIntegrationState(ctx, id, "applied", current.AppliedBackupID); err != nil {
			return ApplyResult{}, err
		}
		return out, nil
	}
	backupID := newIntegrationID()
	dir := filepath.Join(s.DataDir, "backups", id, time.Now().UTC().Format("20060102T150405.000000000"))
	if err = os.MkdirAll(dir, 0700); err != nil {
		return ApplyResult{}, err
	}
	manifest := backupManifest{ID: backupID, IntegrationID: id, CreatedAtMS: time.Now().UnixMilli()}
	for i, f := range ip.Draft.Files {
		backupPath := ""
		if f.Exists {
			backupPath = filepath.Join(dir, fmt.Sprintf("file-%d.backup", i))
			if err = os.WriteFile(backupPath, f.Original, 0600); err != nil {
				return ApplyResult{}, err
			}
		}
		manifest.Files = append(manifest.Files, backupFile{
			Path: f.Path, BackupPath: backupPath, Existed: f.Exists,
			BeforeHash: f.SHA256Before, AfterHash: f.SHA256After, Mode: f.Permissions,
		})
	}
	manifestBytes, _ := json.Marshal(manifest)
	if err = os.WriteFile(filepath.Join(dir, "manifest.json"), manifestBytes, 0600); err != nil {
		return ApplyResult{}, err
	}
	if _, err = s.DB.AddIntegrationBackup(ctx, control.IntegrationBackup{
		ID: backupID, IntegrationID: id, CreatedAtMS: manifest.CreatedAtMS,
		ManifestJSON: string(manifestBytes),
	}); err != nil {
		return ApplyResult{}, err
	}
	out.BackupID = backupID
	for _, f := range ip.Draft.Files {
		if err = writeAtomic(f.Path, f.Content, f.Permissions); err != nil {
			s.restoreFiles(ip.Draft.Files)
			return ApplyResult{}, err
		}
		out.ChangedFiles = append(out.ChangedFiles, f.Path)
	}
	for _, e := range ip.Draft.Env {
		value := ip.Draft.EnvValues[e.Name]
		if err = s.SetEnv(e.Name, value); err != nil {
			s.restoreFiles(ip.Draft.Files)
			return ApplyResult{}, err
		}
		out.EnvChanged = append(out.EnvChanged, e.Name)
	}
	if err = s.DB.SetIntegrationState(ctx, id, "applied", backupID); err != nil {
		return ApplyResult{}, err
	}
	return out, nil
}

func (s *Service) Verify(ctx context.Context, id string) (VerifyResult, error) {
	i, err := s.DB.GetIntegration(ctx, id)
	if err != nil {
		return VerifyResult{}, err
	}
	p, err := s.DB.GetProvider(ctx, i.ProviderConfigID)
	if err != nil {
		return VerifyResult{}, err
	}
	b := BindingFrom(i, p)
	a, _ := s.adapter(i.AgentType)
	out := VerifyResult{Checks: []Check{}}
	if a != nil {
		out.Checks = append(out.Checks, a.VerifyConfig(b))
	}
	if i.AgentType == AgentCodex {
		name := codexEnvName(b)
		v, envErr := s.GetEnv(name)
		check := Check{Name: "env_persisted", Status: "pass", Detail: name}
		if envErr != nil {
			check.Status, check.Detail = "fail", "user environment value is missing"
		} else if hashToken(v) != i.TokenHash {
			check.Status, check.Detail = "fail", "persisted environment value does not match this integration"
		}
		out.Checks = append(out.Checks, check)
	}
	_, _, keyErr := s.DB.ActiveSecretFor(ctx, p.ID, s.Vault)
	check := Check{Name: "provider_key", Status: "pass"}
	if keyErr != nil {
		check.Status, check.Detail = "fail", control.ScrubText(keyErr.Error())
	}
	out.Checks = append(out.Checks, check)

	s.mu.Lock()
	token := s.secrets[id]
	s.mu.Unlock()
	if token == "" && i.AgentType == AgentCodex {
		token, _ = s.GetEnv(codexEnvName(b))
	}
	if token == "" {
		out.Checks = append(out.Checks, Check{Name: "gateway", Status: "skipped",
			Detail: "the token is not available in this process; perform a real request"})
	} else {
		out.Checks = append(out.Checks, s.checkGateway(ctx, b.GatewayBaseURL, token))
	}
	traffic := Check{Name: "agent_traffic", Status: "pending",
		Detail: "waiting for the first real inference request"}
	if i.FirstSeenAtMS != nil {
		traffic.Status, traffic.Detail = "pass", "first request recorded"
	}
	out.Checks = append(out.Checks, traffic)
	state := "applied"
	for _, c := range out.Checks {
		if c.Status == "fail" {
			state = "needs_attention"
			break
		}
	}
	if i.FirstSeenAtMS != nil && state != "needs_attention" {
		state = "verified"
	}
	if err = s.DB.SetIntegrationState(ctx, id, state, ""); err != nil {
		return VerifyResult{}, err
	}
	out.State = state
	return out, nil
}

func (s *Service) checkGateway(ctx context.Context, base, token string) Check {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/models", nil)
	if err != nil {
		return Check{Name: "gateway", Status: "fail", Detail: "invalid gateway URL"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.Client.Do(req)
	if err != nil {
		return Check{Name: "gateway", Status: "fail", Detail: "gateway is not reachable"}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Check{Name: "gateway", Status: "fail", Detail: fmt.Sprintf("gateway returned HTTP %d", resp.StatusCode)}
	}
	return Check{Name: "gateway", Status: "pass", Detail: "token and provider key accepted"}
}

func (s *Service) Rollback(ctx context.Context, id string) (RollbackResult, error) {
	i, err := s.DB.GetIntegration(ctx, id)
	if err != nil {
		return RollbackResult{}, err
	}
	if i.AppliedBackupID == "" {
		if err = s.DB.SetIntegrationState(ctx, id, "disconnected", ""); err != nil {
			return RollbackResult{}, err
		}
		return RollbackResult{State: "disconnected"}, nil
	}
	b, err := s.DB.GetIntegrationBackup(ctx, i.AppliedBackupID)
	if err != nil {
		return RollbackResult{}, err
	}
	var m backupManifest
	if err = json.Unmarshal([]byte(b.ManifestJSON), &m); err != nil {
		return RollbackResult{}, err
	}
	out := RollbackResult{State: "disconnected"}
	for _, f := range m.Files {
		if !safePathUnder(s.Home, f.Path) ||
			(f.BackupPath != "" && !safePathUnder(filepath.Join(s.DataDir, "backups"), f.BackupPath)) {
			return RollbackResult{}, fmt.Errorf("backup manifest contains an unsafe path")
		}
		got, err := hashFile(f.Path)
		if err != nil {
			return RollbackResult{}, err
		}
		if got != f.AfterHash {
			out.Conflicts = append(out.Conflicts, f.Path)
			continue
		}
		if f.Existed {
			content, err := os.ReadFile(f.BackupPath)
			if err != nil {
				return RollbackResult{}, err
			}
			if err = writeAtomic(f.Path, content, f.Mode); err != nil {
				return RollbackResult{}, err
			}
			out.Restored = append(out.Restored, f.Path)
		} else if err = os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
			return RollbackResult{}, err
		} else {
			out.Restored = append(out.Restored, f.Path+" (removed)")
		}
	}
	if len(out.Conflicts) == 0 {
		if i.AgentType == AgentCodex {
			_ = s.UnsetEnv(codexEnvName(Binding{
				IntegrationID: i.ID, AgentType: i.AgentType, Label: i.Label,
			}))
		}
		if err = s.DB.SetIntegrationState(ctx, id, "disconnected", ""); err != nil {
			return RollbackResult{}, err
		}
	}
	return out, nil
}

func (s *Service) Rotate(ctx context.Context, id string) (string, error) {
	i, err := s.DB.GetIntegration(ctx, id)
	if err != nil {
		return "", err
	}
	if i.Revoked() || i.State == "disconnected" {
		return "", fmt.Errorf("%w: cannot rotate a disconnected integration", control.ErrConflict)
	}
	p, err := s.DB.GetProvider(ctx, i.ProviderConfigID)
	if err != nil {
		return "", err
	}
	token, hash, prefix, err := issueToken()
	if err != nil {
		return "", err
	}
	var oldEnv string
	envName := ""
	if i.AgentType == AgentCodex {
		envName = codexEnvName(BindingFrom(i, p))
		oldEnv, _ = s.GetEnv(envName)
		if err = s.SetEnv(envName, token); err != nil {
			return "", err
		}
	}
	if err = s.DB.SetIntegrationToken(ctx, i.ID, hash, prefix); err != nil {
		if envName != "" {
			if oldEnv == "" {
				_ = s.UnsetEnv(envName)
			} else {
				_ = s.SetEnv(envName, oldEnv)
			}
		}
		return "", err
	}
	s.mu.Lock()
	s.secrets[id] = token
	s.mu.Unlock()
	return token, nil
}

func (s *Service) Disconnect(ctx context.Context, id string, rollback bool) (RollbackResult, error) {
	var out RollbackResult
	var err error
	if rollback {
		out, err = s.Rollback(ctx, id)
		if err != nil {
			return out, err
		}
	}
	if err = s.DB.RevokeIntegration(ctx, id); err != nil {
		return out, err
	}
	s.mu.Lock()
	delete(s.secrets, id)
	s.mu.Unlock()
	out.State = "disconnected"
	return out, nil
}

func (s *Service) Authenticate(ctx context.Context, token string) (gateway.Binding, error) {
	hash := hashToken(token)
	i, err := s.DB.IntegrationByTokenHash(ctx, hash)
	if errors.Is(err, control.ErrNotFound) {
		return gateway.Binding{}, gateway.ErrInvalidToken
	}
	if err != nil || subtle.ConstantTimeCompare([]byte(hash), []byte(i.TokenHash)) != 1 {
		return gateway.Binding{}, gateway.ErrInvalidToken
	}
	if i.Revoked() || i.State == "disconnected" {
		return gateway.Binding{}, gateway.ErrInvalidToken
	}
	p, err := s.DB.GetProvider(ctx, i.ProviderConfigID)
	if err != nil || !p.Enabled {
		return gateway.Binding{}, gateway.ErrProviderUnavailable
	}
	return GatewayBinding(BindingFrom(i, p)), nil
}

func GatewayBinding(b Binding) gateway.Binding {
	return gateway.Binding{
		IntegrationID: b.IntegrationID, AgentType: b.AgentType, Label: b.Label,
		ProviderID: b.ProviderID, ProviderName: b.ProviderName, ProviderKind: b.ProviderKind,
		Model: b.Model, CaptureMode: b.CaptureMode, GatewayBaseURL: b.GatewayBaseURL,
	}
}

func BindingFrom(i control.Integration, p control.Provider) Binding {
	return Binding{
		IntegrationID: i.ID, AgentType: i.AgentType, Label: i.Label, ProviderID: p.ID,
		ProviderName: p.Name, ProviderKind: p.Kind, Model: i.Model, CaptureMode: i.CaptureMode,
		GatewayBaseURL: i.GatewayBaseURL,
	}
}

func (s *Service) MarkTraffic(ctx context.Context, id string) error {
	return s.DB.MarkIntegrationTraffic(ctx, id)
}

func (s *Service) MarkAttention(ctx context.Context, id string) error {
	return s.DB.SetIntegrationState(ctx, id, "needs_attention", "")
}

func (s *Service) gatewayURL() string {
	addr := s.Config.Server.ProxyAddr
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	host = strings.Trim(host, "[]")
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return "http://" + host + ":" + port + "/gw/v1"
}

func newIntegrationID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("integration: no entropy")
	}
	return "int_" + hex.EncodeToString(b[:])
}

func issueToken() (token, hash, prefix string, err error) {
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", "", "", err
	}
	token = "obs_gw_" + base64.RawURLEncoding.EncodeToString(b[:])
	if len(token) < 8 {
		return "", "", "", errors.New("short token")
	}
	return token, hashToken(token), token[:8], nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func hashFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return hashBytes(nil), nil
	}
	if err != nil {
		return "", err
	}
	return hashBytes(b), nil
}

type backupManifest struct {
	ID            string       `json:"id"`
	IntegrationID string       `json:"integration_id"`
	CreatedAtMS   int64        `json:"created_at_ms"`
	Files         []backupFile `json:"files"`
}

type backupFile struct {
	Path       string `json:"path"`
	BackupPath string `json:"backup_path"`
	Existed    bool   `json:"existed"`
	BeforeHash string `json:"sha256_before"`
	AfterHash  string `json:"sha256_after"`
	Mode       string `json:"mode"`
}

func writeAtomic(path string, content []byte, modeText string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".observable-config-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err = tmp.Write(content); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	mode := os.FileMode(0644)
	if parsed, parseErr := strconvParseOctal(modeText); parseErr == nil && parsed != 0 {
		mode = parsed
	}
	if err = os.Chmod(name, mode); err != nil {
		return err
	}
	for attempt := 0; attempt < 5; attempt++ {
		if err = atomicReplace(name, path); err == nil {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return err
}

func strconvParseOctal(s string) (os.FileMode, error) {
	var v uint32
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%o", &v)
	return os.FileMode(v), err
}

func (s *Service) restoreFiles(files []fileDraft) {
	for _, f := range files {
		if f.Exists {
			_ = writeAtomic(f.Path, f.Original, f.Permissions)
		} else {
			_ = os.Remove(f.Path)
		}
	}
}
