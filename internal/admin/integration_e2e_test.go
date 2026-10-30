package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ai-observable/ai-observable/internal/config"
	"github.com/ai-observable/ai-observable/internal/control"
	"github.com/ai-observable/ai-observable/internal/integration"
	"github.com/ai-observable/ai-observable/internal/proxy"
	"github.com/ai-observable/ai-observable/internal/store"
	"github.com/ai-observable/ai-observable/internal/vault"
)

func TestIntegrationCreateApplyAndGatewayTraffic(t *testing.T) {
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-provider" {
			t.Errorf("upstream auth = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"deepseek-flash","usage":{"prompt_tokens":7,"completion_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "metrics"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	db, err := control.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	v, err := vault.Open(filepath.Join(dir, "keys"), "file")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := db.CreateProvider(ctx, control.Provider{
		Name: "deepseek", Kind: "deepseek", BaseURL: upstream.URL, AuthStyle: "bearer",
		AuthMode: "managed", StreamUsage: true, Enabled: true, Origin: "ui",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.AddCredential(ctx, provider.ID, "test", "", "sk-provider", v); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Server.ProxyAddr = ln.Addr().String()
	svc := integration.New(db, st, v, cfg, dir)
	reg := proxy.NewControlRegistry(db, v, cfg)
	gw := proxy.NewWithResolver(cfg, st, reg)
	gw.Gateway = svc
	gwSrv := &http.Server{Handler: gw}
	go gwSrv.Serve(ln)
	t.Cleanup(func() { gwSrv.Close() })

	adminSrv := New(Deps{Config: cfg, Store: st, Control: db, Vault: v,
		Integrations: svc, DataDir: dir, GatewayModels: gw.ModelsSeen})

	createBody := `{"agent_type":"cline","label":"e2e-agent","provider_id":"` + provider.ID +
		`","model":"deepseek-flash","capture_mode":"none"}`
	rec := securityRequest(t, adminSrv, "POST", "/api/integrations", createBody, map[string]string{
		"Content-Type": "application/json", "X-LLMTAP-CSRF": adminSrv.CSRF(),
	})
	if rec.Code != 200 {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	var created integration.CreateResult
	if err = json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Token, "obs_gw_") || created.Integration.ID == "" {
		t.Fatal("create did not return a one-time token and integration")
	}
	applyBody, _ := json.Marshal(integration.ApplyRequest{
		PlanID: created.Plan.ID, PlanHash: created.Plan.PlanHash,
	})
	rec = securityRequest(t, adminSrv, "POST", "/api/integrations/"+created.Integration.ID+"/apply",
		string(applyBody), map[string]string{
			"Content-Type": "application/json", "X-LLMTAP-CSRF": adminSrv.CSRF(),
		})
	if rec.Code != 200 {
		t.Fatalf("apply status=%d body=%s", rec.Code, rec.Body.String())
	}

	req, _ := http.NewRequest("POST", "http://"+cfg.Server.ProxyAddr+"/gw/v1/chat/completions",
		bytes.NewBufferString(`{"model":"deepseek-flash","messages":[]}`))
	req.Host = "127.0.0.1"
	req.Header.Set("Authorization", "Bearer "+created.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("gateway status=%d", resp.StatusCode)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM calls WHERE integration_id=?`,
			created.Integration.ID).Scan(&n); err == nil && n == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var project, agent string
	var input int64
	if err = st.DB.QueryRow(`SELECT COALESCE(project,''),COALESCE(agent_type,''),
		COALESCE(input_tokens,0) FROM calls WHERE integration_id=?`,
		created.Integration.ID).Scan(&project, &agent, &input); err != nil {
		t.Fatal(err)
	}
	if project != "e2e-agent" || agent != "cline" || input != 7 {
		t.Fatalf("attribution project=%q agent=%q input=%d", project, agent, input)
	}

	rec = securityRequest(t, adminSrv, "GET", "/api/integrations", "", nil)
	if rec.Code != 200 {
		t.Fatalf("list status=%d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), created.Token) {
		t.Fatal("integration list exposed the plaintext gateway token")
	}
	if strings.Contains(rec.Body.String(), `"token_hash"`) {
		t.Fatal("integration list exposed the token hash")
	}
	sum := sha256.Sum256([]byte(created.Token))
	stored, err := db.IntegrationByTokenHash(ctx, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if stored.TokenHash == created.Token {
		t.Fatal("token stored in plaintext")
	}
}
