package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ai-observable/ai-observable/internal/config"
	"github.com/ai-observable/ai-observable/internal/store"
)

type fakeResolver struct {
	target Target
	err    error
}

func (f fakeResolver) Target(ctx context.Context, name string) (Target, error) {
	if f.err != nil {
		return Target{}, f.err
	}
	if f.target.Name != "" && f.target.Name != name {
		return Target{}, ErrUnknownProvider
	}
	return f.target, nil
}

// upstream records the last request and replies with body/headers.
type upstream struct {
	*httptest.Server
	mu         sync.Mutex
	lastBody   []byte
	lastHeader http.Header
	lastQuery  string
}

type upstreamRequest struct {
	body   []byte
	header http.Header
	query  string
}

func (u *upstream) request() upstreamRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return upstreamRequest{
		body:   append([]byte(nil), u.lastBody...),
		header: u.lastHeader.Clone(),
		query:  u.lastQuery,
	}
}

func newUpstream(t *testing.T, contentType, body string) *upstream {
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestBody, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.lastBody = requestBody
		u.lastHeader = r.Header.Clone()
		u.lastQuery = r.URL.RawQuery
		u.mu.Unlock()
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Upstream", "1")
		w.WriteHeader(200)
		io.WriteString(w, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func testHandler(t *testing.T, target Target) (*Handler, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	cfg := config.Default()
	h := NewWithResolver(cfg, s, fakeResolver{target: target})
	return h, s
}

func do(t *testing.T, h *Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// waitRows blocks until the async writer has persisted at least n rows.
func waitRows(t *testing.T, s *store.Store) []struct{ In, Out int64 } {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM calls").Scan(&n); err == nil && n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rows, err := s.DB.Query("SELECT COALESCE(input_tokens,-1), COALESCE(output_tokens,-1) FROM calls")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []struct{ In, Out int64 }{}
	for rows.Next() {
		var r struct{ In, Out int64 }
		if err = rows.Scan(&r.In, &r.Out); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func TestBytesAreTransparent(t *testing.T) {
	payload := `{"model":"gpt-4o-mini","messages":[]}`
	reply := `{"id":"x","model":"gpt-4o-mini","usage":{"prompt_tokens":3,"completion_tokens":4}}`
	up := newUpstream(t, "application/json", reply)
	h, _ := testHandler(t, Target{Name: "openai", Kind: "openai", Upstream: up.URL,
		AuthStyle: "bearer", AuthMode: "client"})
	rec := do(t, h, "POST", "/openai/v1/chat/completions", payload,
		map[string]string{"Authorization": "Bearer client-key", "X-Custom": "keep"})
	if rec.Code != 200 || rec.Body.String() != reply {
		t.Fatalf("response not byte-identical: %d %q", rec.Code, rec.Body.String())
	}
	request := up.request()
	if string(request.body) != payload {
		t.Fatalf("upstream body changed: %q", request.body)
	}
	if request.header.Get("Authorization") != "Bearer client-key" {
		t.Fatalf("client mode must pass the client key through, got %q", request.header.Get("Authorization"))
	}
	if request.header.Get("X-Custom") != "keep" {
		t.Fatal("custom header was dropped")
	}
	if request.header.Get("X-Llmtap-Session") != "" {
		t.Fatal("private headers must never reach the upstream")
	}
}

func TestManagedModeInjectsStoredKeyAndHidesClientKey(t *testing.T) {
	up := newUpstream(t, "application/json", `{}`)
	h, _ := testHandler(t, Target{Name: "openai", Kind: "openai", Upstream: up.URL,
		AuthStyle: "bearer", AuthMode: "managed", Secret: "sk-stored", Fingerprint: "abcd1234"})
	if rec := do(t, h, "POST", "/openai/v1/chat/completions", `{}`,
		map[string]string{"Authorization": "Bearer placeholder"}); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if got := up.request().header.Get("Authorization"); got != "Bearer sk-stored" {
		t.Fatalf("managed mode injected %q", got)
	}
}

func TestAuthStyles(t *testing.T) {
	cases := []struct {
		style, header, want, wantQuery string
	}{
		{"bearer", "Authorization", "Bearer s3cret", ""},
		{"x-api-key", "X-Api-Key", "s3cret", ""},
		{"api-key", "Api-Key", "s3cret", ""},
		{"x-goog-api-key", "X-Goog-Api-Key", "s3cret", ""},
		{"query-key", "", "", "key=s3cret"},
		{"none", "Authorization", "", ""},
	}
	for _, c := range cases {
		t.Run(c.style, func(t *testing.T) {
			up := newUpstream(t, "application/json", `{}`)
			h, _ := testHandler(t, Target{Name: "p", Kind: "local", Upstream: up.URL,
				AuthStyle: c.style, AuthMode: "managed", Secret: "s3cret"})
			do(t, h, "GET", "/p/v1/models?limit=2", "", map[string]string{"Authorization": "Bearer client"})
			request := up.request()
			if got := request.header.Get(c.header); c.header != "" && got != c.want {
				t.Fatalf("%s = %q, want %q", c.header, got, c.want)
			}
			if c.style == "query-key" && !strings.Contains(request.query, c.wantQuery) {
				t.Fatalf("query %q missing %q", request.query, c.wantQuery)
			}
			if c.style == "query-key" && !strings.Contains(request.query, "limit=2") {
				t.Fatalf("existing query dropped: %q", request.query)
			}
		})
	}
}

func TestStreamUsageInjectionIsChatCompletionsOnly(t *testing.T) {
	const sse = "data: {\"model\":\"gpt-4o-mini\",\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"
	up := newUpstream(t, "text/event-stream", sse)
	h, s := testHandler(t, Target{Name: "openai", Kind: "openai", Upstream: up.URL,
		AuthStyle: "bearer", AuthMode: "client", StreamUsage: true})
	rec := do(t, h, "POST", "/openai/v1/chat/completions", `{"model":"gpt-4o-mini","stream":true}`, nil)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if request := up.request(); !strings.Contains(string(request.body), `"stream_options":{"include_usage":true}`) {
		t.Fatalf("stream_options not injected: %s", request.body)
	}
	if rec.Body.String() != sse {
		t.Fatalf("stream bytes changed: %q", rec.Body.String())
	}
	// The same flag on a non-chat path must not touch the body.
	do(t, h, "POST", "/openai/v1/messages", `{"model":"claude","stream":true}`, nil)
	if request := up.request(); strings.Contains(string(request.body), "stream_options") {
		t.Fatalf("stream_options leaked into a non-chat endpoint: %s", request.body)
	}
	waitRows(t, s)
}

func TestStreamedChatRecordsUsageModelPricingFinishAndContentTTFT(t *testing.T) {
	const sse = ": keep-alive\r\n\r\n" +
		"data: {\"model\":\"deepseek-flash\",\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\r\n\r\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\r\n\r\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":10," +
		"\"prompt_cache_hit_tokens\":40,\"prompt_cache_miss_tokens\":60,\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\r\n\r\n" +
		"data: [DONE]\r\n\r\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		parts := strings.SplitAfter(sse, "\r\n\r\n")
		for i, part := range parts {
			if i == 1 {
				time.Sleep(60 * time.Millisecond)
			}
			io.WriteString(w, part)
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(up.Close)
	h, s := testHandler(t, Target{Name: "deepseek", Kind: "deepseek", Upstream: up.URL,
		AuthStyle: "bearer", AuthMode: "client", StreamUsage: true})
	if rec := do(t, h, "POST", "/deepseek/v1/chat/completions",
		`{"model":"deepseek-flash","stream":true}`, nil); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM calls").Scan(&n); err == nil && n == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var model, finish string
	var input, output, cacheRead, ttft int64
	var pricingMissing, success int64
	if err := s.DB.QueryRow(`SELECT model_served,input_tokens,output_tokens,cache_read_tokens,
		ttft_ms,finish_reason,pricing_missing,success FROM calls`).
		Scan(&model, &input, &output, &cacheRead, &ttft, &finish, &pricingMissing, &success); err != nil {
		t.Fatal(err)
	}
	if model != "deepseek-flash" || input != 100 || output != 10 || cacheRead != 40 {
		t.Fatalf("usage/model: model=%q input=%d output=%d cache=%d", model, input, output, cacheRead)
	}
	if finish != "stop" || pricingMissing != 0 || success != 1 {
		t.Fatalf("finish=%q pricing_missing=%d success=%d", finish, pricingMissing, success)
	}
	if ttft < 40 {
		t.Fatalf("ttft=%dms measured before generated content", ttft)
	}
}

func TestTruncatedStreamIsRecordedAsInvalidResponse(t *testing.T) {
	const sse = "data: {\"model\":\"deepseek-flash\",\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sse)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(up.Close)
	h, s := testHandler(t, Target{Name: "deepseek", Kind: "deepseek", Upstream: up.URL,
		AuthStyle: "bearer", AuthMode: "client", StreamUsage: true})
	if rec := do(t, h, "POST", "/deepseek/v1/chat/completions",
		`{"model":"deepseek-flash","stream":true}`, nil); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM calls").Scan(&n); err == nil && n == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var success int64
	var errorType string
	if err := s.DB.QueryRow("SELECT success,COALESCE(error_type,'') FROM calls").Scan(&success, &errorType); err != nil {
		t.Fatal(err)
	}
	if success != 0 || errorType != "invalid_response" {
		t.Fatalf("truncated 200 stream recorded success=%d error_type=%q", success, errorType)
	}
}

func TestUnknownProviderDisabledAndUnlockError(t *testing.T) {
	h, _ := testHandler(t, Target{})
	h.R = fakeResolver{err: ErrUnknownProvider}
	if rec := do(t, h, "POST", "/nope/v1/chat/completions", `{}`, nil); rec.Code != 404 {
		t.Fatalf("unknown provider status %d", rec.Code)
	}
	h2, _ := testHandler(t, Target{})
	h2.R = fakeResolver{err: ErrDisabled}
	if rec := do(t, h2, "POST", "/off/v1/chat/completions", `{}`, nil); rec.Code != 403 {
		t.Fatalf("disabled provider status %d", rec.Code)
	}
	h3, _ := testHandler(t, Target{})
	h3.R = fakeResolver{err: ErrNoCredential}
	if rec := do(t, h3, "POST", "/openai/v1/chat/completions", `{}`, nil); rec.Code != 503 {
		t.Fatalf("missing credential status %d", rec.Code)
	}
}

func TestUsageIsRecordedFromAdapter(t *testing.T) {
	// Anthropic reports cache tokens in its own field names.
	reply := `{"model":"claude-sonnet-4-20250514","usage":{"input_tokens":12,"output_tokens":7,` +
		`"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}`
	up := newUpstream(t, "application/json", reply)
	h, s := testHandler(t, Target{Name: "anthropic", Kind: "anthropic", Upstream: up.URL,
		AuthStyle: "x-api-key", AuthMode: "managed", Secret: "sk-ant-x", Fingerprint: "feedface"})
	do(t, h, "POST", "/anthropic/v1/messages", `{"model":"claude-sonnet-4-20250514"}`, nil)
	rows := waitRows(t, s)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].In != 12 || rows[0].Out != 7 {
		t.Fatalf("usage not parsed: %+v", rows[0])
	}
	var served, fp, source, provider string
	if err := s.DB.QueryRow("SELECT model_served,key_fingerprint,source,provider FROM calls").
		Scan(&served, &fp, &source, &provider); err != nil {
		t.Fatal(err)
	}
	if served != "claude-sonnet-4-20250514" || fp != "feedface" || source != "proxy" || provider != "anthropic" {
		t.Fatalf("row provenance wrong: served=%q fp=%q source=%q provider=%q", served, fp, source, provider)
	}
}

func TestInvalidUpstreamIsRejected(t *testing.T) {
	h, _ := testHandler(t, Target{Name: "bad", Kind: "openai", Upstream: "not-a-url", AuthMode: "client"})
	if rec := do(t, h, "POST", "/bad/v1/chat/completions", `{}`, nil); rec.Code != 502 {
		t.Fatalf("status %d", rec.Code)
	}
}
