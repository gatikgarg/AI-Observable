package admin

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ai-observable/ai-observable/internal/config"
)

func securityServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	return New(Deps{Config: cfg})
}

func securityRequest(t *testing.T, s *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:7879"
	req.RemoteAddr = "127.0.0.1:5000"
	for k, v := range headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestManagementGuardsRejectCrossSiteAndGatewayTokens(t *testing.T) {
	s := securityServer(t)
	if rec := securityRequest(t, s, "GET", "/healthz", "", map[string]string{"Host": "evil.example"}); rec.Code != 400 {
		t.Fatalf("foreign Host status=%d", rec.Code)
	}
	if rec := securityRequest(t, s, "POST", "/api/integrations", `{}`, map[string]string{
		"Content-Type": "application/json",
	}); rec.Code != 403 {
		t.Fatalf("missing CSRF status=%d", rec.Code)
	}
	if rec := securityRequest(t, s, "POST", "/api/integrations", `{}`, map[string]string{
		"Content-Type": "application/json", "X-LLMTAP-CSRF": s.CSRF(),
		"Origin": "https://evil.example",
	}); rec.Code != 403 {
		t.Fatalf("foreign Origin status=%d", rec.Code)
	}
	if rec := securityRequest(t, s, "GET", "/api/integrations", "", map[string]string{
		"Authorization": "Bearer obs_gw_should_never_authorize_admin",
	}); rec.Code != 401 {
		t.Fatalf("gateway token on management status=%d", rec.Code)
	}
	if rec := securityRequest(t, s, "POST", "/api/integrations", `{}`, map[string]string{
		"Content-Type": "text/plain", "X-LLMTAP-CSRF": s.CSRF(),
	}); rec.Code != 415 {
		t.Fatalf("non-JSON mutation status=%d", rec.Code)
	}
}
