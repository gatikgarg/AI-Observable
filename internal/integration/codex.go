package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type codexAdapter struct {
	Home string
}

func (a *codexAdapter) ID() string           { return AgentCodex }
func (a *codexAdapter) DisplayName() string  { return "Codex" }
func (a *codexAdapter) RequiredWire() string { return "responses" }
func (a *codexAdapter) SetupMode() string    { return "automatic" }

func (a *codexAdapter) configPath() string {
	if root := strings.TrimSpace(os.Getenv("CODEX_HOME")); root != "" {
		path := filepath.Join(root, "config.toml")
		if safePathUnder(a.Home, path) {
			return path
		}
	}
	return filepath.Join(a.Home, ".codex", "config.toml")
}

func (a *codexAdapter) Detect(context.Context) Detection {
	d := Detection{ConfigPaths: []string{a.configPath()}}
	if _, err := os.Stat(a.configPath()); err == nil {
		d.Installed = true
	}
	if path, err := exec.LookPath("codex"); err == nil {
		d.Installed = true
		if out, err := exec.Command(path, "--version").Output(); err == nil {
			d.Version = strings.TrimSpace(string(out))
		}
	}
	if runtime.GOOS == "windows" {
		d.Notes = append(d.Notes, "Fully quit VS Code after apply so the extension host reloads the user environment.")
	}
	return d
}

func (a *codexAdapter) Plan(_ context.Context, b Binding) (planDraft, error) {
	path := a.configPath()
	if !safePathUnder(a.Home, path) {
		return planDraft{}, fmt.Errorf("Codex config path escapes the user home")
	}
	orig, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return planDraft{}, fmt.Errorf("read Codex config: %w", err)
	}
	if exists {
		var parsed map[string]any
		parseInput := orig
		if bytes.HasPrefix(parseInput, []byte{0xef, 0xbb, 0xbf}) {
			parseInput = parseInput[3:]
		}
		if err = toml.Unmarshal(parseInput, &parsed); err != nil {
			return planDraft{}, fmt.Errorf("Codex config is not valid TOML; use guided setup: %w", err)
		}
		if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm()&0200 == 0 {
			return planDraft{}, fmt.Errorf("Codex config is read-only")
		}
	}
	content, lineEnding, err := managedCodexContent(orig, b)
	if err != nil {
		return planDraft{}, err
	}
	var check struct {
		ModelProvider  string         `toml:"model_provider"`
		Model          string         `toml:"model"`
		ModelProviders map[string]any `toml:"model_providers"`
	}
	if err = toml.Unmarshal(content, &check); err != nil {
		return planDraft{}, fmt.Errorf("generated Codex config is invalid TOML: %w", err)
	}
	providerID := codexProviderID(b)
	if check.ModelProvider != providerID || check.Model != b.Model {
		return planDraft{}, fmt.Errorf("generated Codex top-level provider/model did not resolve")
	}
	if _, ok := check.ModelProviders[providerID]; !ok {
		return planDraft{}, fmt.Errorf("generated Codex provider table is missing")
	}
	before := hashBytes(orig)
	after := hashBytes(content)
	preview := redactManagedPreview(string(content), 2400)
	diff := "--- existing config.toml\n+++ managed config.toml\n" +
		"@@ top-level provider/model and [model_providers." + providerID + "]\n" +
		preview
	perm := "0644"
	if exists {
		if info, statErr := os.Stat(path); statErr == nil {
			perm = fmt.Sprintf("%04o", info.Mode().Perm())
		}
	}
	envName := codexEnvName(b)
	return planDraft{
		Files: []fileDraft{{
			Path: path, Original: append([]byte(nil), orig...), Exists: exists, Content: content,
			LineEnding: lineEnding, Permissions: perm, SHA256Before: before, SHA256After: after,
			Preview: preview, Diff: diff,
		}},
		Env:       []EnvChange{{Name: envName, Scope: "user", Action: "set"}},
		EnvValues: map[string]string{envName: ""},
		Restart: Restart{Required: true, What: []string{
			"Close every running Codex CLI session.",
			"Fully quit VS Code (all windows), then reopen it.",
		}},
		Warnings: []string{
			"Changes the default Codex model provider.",
			"The provider key stays with Observable; only an Observable gateway token is placed in the user environment.",
			"Observable must be running whenever Codex is used, or requests will fail.",
		},
	}, nil
}

func safePathUnder(root, path string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if !relativeInside(rootAbs, pathAbs) {
		return false
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return false
	}
	current := rootAbs
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			return true
		}
		if statErr != nil {
			return false
		}
		if info.Mode()&os.ModeSymlink != 0 {
			real, evalErr := filepath.EvalSymlinks(current)
			if evalErr != nil || !relativeInside(rootAbs, real) {
				return false
			}
		}
	}
	return true
}

func relativeInside(root, path string) bool {
	clean := func(s string) string {
		s = filepath.Clean(strings.TrimPrefix(s, `\\?\`))
		if runtime.GOOS == "windows" {
			s = strings.ToLower(s)
		}
		return s
	}
	rel, err := filepath.Rel(clean(root), clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (a *codexAdapter) VerifyConfig(b Binding) Check {
	path := a.configPath()
	content, err := os.ReadFile(path)
	if err != nil {
		return Check{Name: "codex_config", Status: "fail", Detail: err.Error()}
	}
	needle := "# >>> observable:top id=" + b.IntegrationID
	if !bytes.Contains(content, []byte(needle)) {
		return Check{Name: "codex_config", Status: "fail", Detail: "managed block is missing"}
	}
	var parsed map[string]any
	if err = toml.Unmarshal(content, &parsed); err != nil {
		return Check{Name: "codex_config", Status: "fail", Detail: "managed config is invalid TOML"}
	}
	return Check{Name: "codex_config", Status: "pass", Detail: path}
}

func codexProviderID(b Binding) string {
	short := strings.ReplaceAll(b.IntegrationID, "-", "")
	if len(short) > 8 {
		short = short[:8]
	}
	return "observable_" + safeName(b.Label) + "_" + short
}

func codexEnvName(b Binding) string {
	short := strings.ReplaceAll(b.IntegrationID, "-", "")
	if len(short) > 8 {
		short = short[:8]
	}
	return "OBSERVABLE_KEY_CODEX_" + strings.ToUpper(short)
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "agent"
	}
	return out
}

func managedCodexContent(orig []byte, b Binding) ([]byte, string, error) {
	bom := bytes.HasPrefix(orig, []byte{0xef, 0xbb, 0xbf})
	if bom {
		orig = orig[3:]
	}
	lineEnding := "\n"
	if bytes.Contains(orig, []byte("\r\n")) {
		lineEnding = "\r\n"
	}
	text := strings.ReplaceAll(string(orig), "\r\n", "\n")
	text = stripManagedTop(text)
	text = stripManagedProvider(text, b.IntegrationID)
	lines := strings.Split(text, "\n")
	firstTable := len(lines)
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			firstTable = i
			break
		}
	}
	keyRe := regexp.MustCompile(`^\s*(model|model_provider)\s*=`)
	for i := 0; i < firstTable; i++ {
		if keyRe.MatchString(lines[i]) {
			lines[i] = "# observable original: " + lines[i]
		}
	}
	body := strings.TrimLeft(strings.Join(lines, "\n"), "\n")
	providerID := codexProviderID(b)
	envName := codexEnvName(b)
	top := fmt.Sprintf(`# >>> observable:top id=%s (managed by Observable; do not edit) >>>
model_provider = %s
model = %s
# <<< observable:top id=%s <<<`,
		b.IntegrationID, strconv.Quote(providerID), strconv.Quote(b.Model), b.IntegrationID)
	provider := fmt.Sprintf(`# >>> observable:provider id=%s (managed by Observable; do not edit) >>>
[model_providers.%s]
name = %s
base_url = %s
env_key = %s
wire_api = "responses"
# <<< observable:provider id=%s <<<`,
		b.IntegrationID, providerID, strconv.Quote("Observable -> "+b.ProviderName),
		strconv.Quote(strings.TrimRight(b.GatewayBaseURL, "/")), strconv.Quote(envName), b.IntegrationID)
	out := top + "\n\n"
	if strings.TrimSpace(body) != "" {
		out += strings.TrimRight(body, "\n") + "\n\n"
	}
	out += provider + "\n"
	out = strings.ReplaceAll(out, "\n", lineEnding)
	return []byte(out), lineEnding, nil
}

func stripManagedTop(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	skipping := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "# >>> observable:top id=") {
			skipping = true
			continue
		}
		if skipping {
			if strings.HasPrefix(trim, "# <<< observable:top id=") {
				skipping = false
			}
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func stripManagedProvider(text, id string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	begin := "# >>> observable:provider id=" + id
	end := "# <<< observable:provider id=" + id
	skipping := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, begin) {
			skipping = true
			continue
		}
		if skipping {
			if strings.HasPrefix(trim, end) {
				skipping = false
			}
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func redactManagedPreview(s string, max int) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "key") || strings.Contains(lower, "token") ||
			strings.Contains(lower, "secret") || strings.Contains(lower, "authorization") {
			if strings.Contains(line, "=") {
				parts := strings.SplitN(line, "=", 2)
				lines[i] = parts[0] + `= "[REDACTED]"`
			}
		}
	}
	out := strings.Join(lines, "\n")
	if len(out) > max {
		out = out[:max] + "\n..."
	}
	return out
}
