package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-observable/ai-observable/internal/gateway"
)

type fakeGateway struct {
	token   string
	binding gateway.Binding
	traffic atomic.Int64
}

func (f *fakeGateway) Authenticate(_ context.Context, token string) (gateway.Binding, error) {
	if token != f.token {
		return gateway.Binding{}, gateway.ErrInvalidToken
	}
	return f.binding, nil
}

func (f *fakeGateway) MarkTraffic(context.Context, string) error {
	f.traffic.Add(1)
	return nil
}

func (f *fakeGateway) MarkAttention(context.Context, string) error { return nil }

func gatewayRequest(t *testing.T, h *Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:7878"
	req.RemoteAddr = "127.0.0.1:5555"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGatewayRecordsAttributionAndHidesGatewayToken(t *testing.T) {
	reply := `{"id":"x","model":"deepseek-flash","usage":{"prompt_tokens":9,"completion_tokens":4}}`
	up := newUpstream(t, "application/json", reply)
	h, s := testHandler(t, Target{Name: "deepseek", Kind: "deepseek", Upstream: up.URL,
		AuthStyle: "bearer", AuthMode: "managed", Secret: "sk-provider", StreamUsage: true})
	h.Gateway = &fakeGateway{token: "obs_gw_test-token-value",
		binding: gateway.Binding{IntegrationID: "int_test", AgentType: "cline", Label: "cline-main",
			ProviderName: "deepseek", ProviderID: "p1", CaptureMode: "none"}}
	rec := gatewayRequest(t, h, "POST", "/gw/v1/chat/completions",
		`{"model":"deepseek-flash","messages":[{"role":"user","content":"secret prompt"}]}`,
		map[string]string{"Authorization": "Bearer obs_gw_test-token-value",
			"X-Llmtap-Project": "client-attribution"})
	if rec.Code != 200 || rec.Body.String() != reply {
		t.Fatalf("response: %d %q", rec.Code, rec.Body.String())
	}
	if got := up.lastHeader.Get("Authorization"); got != "Bearer sk-provider" {
		t.Fatalf("upstream auth = %q", got)
	}
	for _, v := range up.lastHeader.Values("Authorization") {
		if strings.Contains(v, "obs_gw_") {
			t.Fatal("gateway token reached upstream")
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM calls").Scan(&n); err == nil && n == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var integrationID, agent, project, requestPreview, responsePreview, errorMessage string
	var input int64
	if err := s.DB.QueryRow(`SELECT COALESCE(integration_id,''),COALESCE(agent_type,''),
		COALESCE(project,''),COALESCE(request_preview,''),COALESCE(response_preview,''),
		COALESCE(error_message,''),COALESCE(input_tokens,0) FROM calls`).
		Scan(&integrationID, &agent, &project, &requestPreview, &responsePreview, &errorMessage, &input); err != nil {
		t.Fatal(err)
	}
	if integrationID != "int_test" || agent != "cline" || project != "cline-main" || input != 9 {
		t.Fatalf("attribution: integration=%q agent=%q project=%q input=%d", integrationID, agent, project, input)
	}
	if requestPreview != "" || responsePreview != "" || errorMessage != "" {
		t.Fatal("metadata-only gateway traffic stored content")
	}
	if h.Gateway.(*fakeGateway).traffic.Load() != 1 {
		t.Fatal("traffic was not marked verified")
	}
}

func TestGatewayModelsPassthroughIsNotRecorded(t *testing.T) {
	models := `{"object":"list","data":[{"id":"deepseek-flash"}]}`
	up := newUpstream(t, "application/json", models)
	h, s := testHandler(t, Target{Name: "deepseek", Kind: "deepseek", Upstream: up.URL,
		AuthStyle: "bearer", AuthMode: "managed", Secret: "sk-provider"})
	h.Gateway = &fakeGateway{token: "obs_gw_models", binding: gateway.Binding{
		IntegrationID: "int_models", AgentType: "kilo-code", Label: "kilo",
		ProviderName: "deepseek", ProviderID: "p1", CaptureMode: "none"}}
	rec := gatewayRequest(t, h, "GET", "/gw/v1/models", "",
		map[string]string{"x-api-key": "obs_gw_models"})
	if rec.Code != 200 || rec.Body.String() != models {
		t.Fatalf("models response: %d %q", rec.Code, rec.Body.String())
	}
	time.Sleep(350 * time.Millisecond)
	var calls int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM calls").Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || h.ModelsSeen() != 1 {
		t.Fatalf("models calls=%d counter=%d", calls, h.ModelsSeen())
	}
}

func TestGatewayResponsesStreamUsesTerminalUsage(t *testing.T) {
	const sse = "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"deepseek-flash\",\"usage\":{\"input_tokens\":20,\"output_tokens\":5}}}\n\n"
	up := newUpstream(t, "text/event-stream", sse)
	h, s := testHandler(t, Target{Name: "deepseek", Kind: "deepseek", Upstream: up.URL,
		AuthStyle: "bearer", AuthMode: "managed", Secret: "sk-provider"})
	h.Gateway = &fakeGateway{token: "obs_gw_responses", binding: gateway.Binding{
		IntegrationID: "int_resp", AgentType: "codex", Label: "codex",
		ProviderName: "deepseek", ProviderID: "p1", CaptureMode: "none"}}
	payload := `{"model":"deepseek-flash","input":"hi","stream":true,"tools":[{"type":"function","name":"edit_file","parameters":{"type":"object"}}]}`
	rec := gatewayRequest(t, h, "POST", "/gw/v1/responses", payload,
		map[string]string{"Authorization": "Bearer obs_gw_responses"})
	if rec.Code != 200 || rec.Body.String() != sse {
		t.Fatalf("responses response changed: %d %q", rec.Code, rec.Body.String())
	}
	if string(up.lastBody) != payload {
		t.Fatal("Responses request body changed")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM calls").Scan(&n); err == nil && n == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var input, output, success int64
	var model, errorType string
	if err := s.DB.QueryRow("SELECT COALESCE(input_tokens,0),COALESCE(output_tokens,0),success,COALESCE(model_served,''),COALESCE(error_type,'') FROM calls").
		Scan(&input, &output, &success, &model, &errorType); err != nil {
		t.Fatal(err)
	}
	if input != 20 || output != 5 || success != 1 || model != "deepseek-flash" {
		t.Fatalf("responses usage input=%d output=%d success=%d model=%q error=%q", input, output, success, model, errorType)
	}
}

func TestGatewayRejectsBadTokenOriginAndPath(t *testing.T) {
	up := newUpstream(t, "application/json", `{}`)
	h, _ := testHandler(t, Target{Name: "deepseek", Kind: "deepseek", Upstream: up.URL,
		AuthMode: "managed", Secret: "sk-provider"})
	fg := &fakeGateway{token: "obs_gw_good", binding: gateway.Binding{
		IntegrationID: "i", AgentType: "cline", Label: "c", ProviderName: "deepseek", ProviderID: "p"}}
	h.Gateway = fg
	rec := gatewayRequest(t, h, "POST", "/gw/v1/chat/completions", `{}`,
		map[string]string{"Authorization": "Bearer bad"})
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), `"code":"invalid_api_key"`) {
		t.Fatalf("bad token: %d %s", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest("POST", "/gw/v1/chat/completions", strings.NewReader(`{}`))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Authorization", "Bearer obs_gw_good")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("foreign origin status=%d", rr.Code)
	}
	rec = gatewayRequest(t, h, "POST", "/gw/v1/billing/admin", `{}`,
		map[string]string{"Authorization": "Bearer obs_gw_good"})
	if rec.Code != 404 {
		t.Fatalf("non-allowlisted path status=%d", rec.Code)
	}
}

func TestGatewayConcurrentCallsAllRecorded(t *testing.T) {
	reply := `{"model":"deepseek-flash","usage":{"prompt_tokens":1,"completion_tokens":1}}`
	up := newUpstream(t, "application/json", reply)
	h, s := testHandler(t, Target{Name: "deepseek", Kind: "deepseek", Upstream: up.URL,
		AuthStyle: "bearer", AuthMode: "managed", Secret: "sk-provider"})
	h.Gateway = &fakeGateway{token: "obs_gw_concurrent", binding: gateway.Binding{
		IntegrationID: "int_burst", AgentType: "cline", Label: "burst",
		ProviderName: "deepseek", ProviderID: "p1", CaptureMode: "none"}}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := gatewayRequest(t, h, "POST", "/gw/v1/chat/completions",
				`{"model":"deepseek-flash","messages":[]}`,
				map[string]string{"Authorization": "Bearer obs_gw_concurrent"})
			if rec.Code != 200 {
				t.Errorf("concurrent status %d", rec.Code)
			}
		}()
	}
	wg.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM calls").Scan(&n); err == nil && n == 50 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	var n int64
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM calls").Scan(&n)
	t.Fatalf("concurrent calls recorded=%d, want 50 (dropped=%d)", n, s.Dropped())
}

func TestGatewayMetadataOnlyOmitsUpstreamErrorMessage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"type":"rate_limit","message":"prompt was: top secret"}}`)
	}))
	t.Cleanup(up.Close)
	h, s := testHandler(t, Target{Name: "deepseek", Kind: "deepseek", Upstream: up.URL,
		AuthStyle: "bearer", AuthMode: "managed", Secret: "sk-provider"})
	h.Gateway = &fakeGateway{token: "obs_gw_error", binding: gateway.Binding{
		IntegrationID: "int_error", AgentType: "cline", Label: "errors",
		ProviderName: "deepseek", ProviderID: "p1", CaptureMode: "none"}}
	rec := gatewayRequest(t, h, "POST", "/gw/v1/chat/completions", `{"model":"deepseek-flash"}`,
		map[string]string{"Authorization": "Bearer obs_gw_error"})
	if rec.Code != 429 || !strings.Contains(rec.Body.String(), "top secret") {
		t.Fatal("upstream error was not relayed unchanged")
	}
	time.Sleep(350 * time.Millisecond)
	var message, preview string
	if err := s.DB.QueryRow("SELECT COALESCE(error_message,''),COALESCE(request_preview,'') FROM calls").
		Scan(&message, &preview); err != nil {
		t.Fatal(err)
	}
	if message != "" || preview != "" {
		t.Fatalf("metadata-only capture leaked content: message=%q preview=%q", message, preview)
	}
}

func TestGatewayToolPayloadBytesAreTransparent(t *testing.T) {
	payload := `{"model":"deepseek-flash","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"write","arguments":"{\"x\":1}"}}]}]}`
	reply := `{"id":"tool","model":"deepseek-flash","choices":[{"message":{"tool_calls":[{"id":"call_2","type":"function","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`
	up := newUpstream(t, "application/json", reply)
	h, _ := testHandler(t, Target{Name: "deepseek", Kind: "deepseek", Upstream: up.URL,
		AuthStyle: "bearer", AuthMode: "managed", Secret: "sk-provider"})
	h.Gateway = &fakeGateway{token: "obs_gw_tools", binding: gateway.Binding{
		IntegrationID: "int_tools", AgentType: "cline", Label: "tools",
		ProviderName: "deepseek", ProviderID: "p1", CaptureMode: "none"}}
	rec := gatewayRequest(t, h, "POST", "/gw/v1/chat/completions", payload,
		map[string]string{"Authorization": "Bearer obs_gw_tools"})
	if rec.Code != 200 || rec.Body.String() != reply || string(up.lastBody) != payload {
		t.Fatal("tool-call bytes changed")
	}
}

func TestGatewayForwardsFiveMegabyteRequest(t *testing.T) {
	up := newUpstream(t, "application/json", `{"model":"deepseek-flash"}`)
	h, _ := testHandler(t, Target{Name: "deepseek", Kind: "deepseek", Upstream: up.URL,
		AuthStyle: "bearer", AuthMode: "managed", Secret: "sk-provider"})
	h.Gateway = &fakeGateway{token: "obs_gw_large", binding: gateway.Binding{
		IntegrationID: "int_large", AgentType: "cline", Label: "large",
		ProviderName: "deepseek", ProviderID: "p1", CaptureMode: "none"}}
	payload := `{"model":"deepseek-flash","padding":"` + strings.Repeat("x", 5<<20) + `"}`
	rec := gatewayRequest(t, h, "POST", "/gw/v1/chat/completions", payload,
		map[string]string{"Authorization": "Bearer obs_gw_large"})
	if rec.Code != 200 || len(up.lastBody) != len(payload) || string(up.lastBody) != payload {
		t.Fatalf("large request changed: status=%d sent=%d got=%d", rec.Code, len(payload), len(up.lastBody))
	}
}

func TestOpenAIErrorHasStableShape(t *testing.T) {
	rec := httptest.NewRecorder()
	openAIError(rec, http.StatusUnauthorized, "invalid_api_key", "readable")
	var out struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Error.Message != "readable" || out.Error.Code != "invalid_api_key" || out.Error.Type == "" {
		t.Fatalf("error shape = %+v", out.Error)
	}
}
