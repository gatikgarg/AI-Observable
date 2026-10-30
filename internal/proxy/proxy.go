package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ai-observable/ai-observable/internal/config"
	"github.com/ai-observable/ai-observable/internal/pricing"
	"github.com/ai-observable/ai-observable/internal/session"
	"github.com/ai-observable/ai-observable/internal/store"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ai-observable/ai-observable/internal/gateway"
)

var secret = regexp.MustCompile(`(?i)obs_gw_[A-Za-z0-9_-]{20,}|sk-[a-z0-9_-]{16,}|bearer\s+\S+|AIza[0-9A-Za-z_-]{20,}`)

type GatewayResolver interface {
	Authenticate(context.Context, string) (gateway.Binding, error)
	MarkTraffic(context.Context, string) error
	MarkAttention(context.Context, string) error
}

type Handler struct {
	C        config.Config
	S        *store.Store
	Sessions *session.Manager
	Client   *http.Client
	// R resolves providers at request time (control DB). Nil falls back to the
	// providers declared in config.toml.
	R       Resolver
	Gateway GatewayResolver

	gwMu       sync.Mutex
	gwFailures map[string]gatewayFailures
	modelsSeen int64
}

func New(c config.Config, s *store.Store) *Handler {
	return &Handler{C: c, S: s, Sessions: session.New(), gwFailures: map[string]gatewayFailures{},
		Client: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 120 * time.Second}, Timeout: 0}}
}

// NewWithResolver builds a handler that looks providers up per request.
func NewWithResolver(c config.Config, s *store.Store, r Resolver) *Handler {
	h := New(c, s)
	h.R = r
	return h
}

func (h *Handler) resolve(ctx context.Context, name string) (Target, error) {
	if h.R != nil {
		return h.R.Target(ctx, name)
	}
	p, ok := h.C.Providers[name]
	if !ok {
		return Target{}, fmt.Errorf("%w: %s", ErrUnknownProvider, name)
	}
	return Target{Name: name, Kind: name, Upstream: p.Upstream, AuthStyle: "bearer",
		AuthMode: "client", StreamUsage: p.StreamUsage}, nil
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrUnknownProvider):
		return http.StatusNotFound
	case errors.Is(err, ErrDisabled):
		return http.StatusForbidden
	case errors.Is(err, ErrNoCredential), errors.Is(err, ErrVaultLocked):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func writeErr(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":%q}`, err.Error())
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if x := recover(); x != nil {
			fmt.Println("llmtap proxy panic", x)
			http.Error(w, "proxy error", 500)
		}
	}()
	if strings.HasPrefix(r.URL.Path, "/gw/v1/") {
		h.serveGateway(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) < 2 {
		writeErr(w, http.StatusNotFound, errors.New("provider required"))
		return
	}
	t, e := h.resolve(r.Context(), parts[0])
	if e != nil {
		writeErr(w, statusFor(e), e)
		return
	}
	u, e := url.Parse(t.Upstream)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("provider %s has an invalid upstream", t.Name))
		return
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + strings.Join(parts[1:], "/")
	u.RawQuery = r.URL.RawQuery
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	model, stream := requestInfo(body)
	if stream && t.StreamUsage && strings.HasSuffix(u.Path, "/chat/completions") {
		body = injectUsage(body)
	}
	req, e := http.NewRequestWithContext(r.Context(), r.Method, u.String(), bytes.NewReader(body))
	if e != nil {
		writeErr(w, http.StatusBadGateway, e)
		return
	}
	for k, v := range r.Header {
		if hop(k) || strings.EqualFold(k, "Accept-Encoding") || privateHeader(k) {
			continue
		}
		req.Header[k] = append([]string(nil), v...)
	}
	applyAuth(req, t)
	for k, v := range t.Extra {
		if k == "" || privateHeader(k) || hop(k) {
			continue
		}
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, e := h.Client.Do(req)
	if e != nil {
		et := "connection_error"
		if strings.Contains(strings.ToLower(e.Error()), "timeout") {
			et = "timeout"
		}
		if r.Context().Err() != nil {
			et = "client_cancelled"
		}
		status := 0
		if et == "client_cancelled" {
			status = 499
		}
		h.record(r, t, u.Path, model, stream, status, false, et, e.Error(), start, 0, body, nil, nil)
		http.Error(w, e.Error(), 502)
		return
	}

	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	var out bytes.Buffer
	firstByte := int64(0)
	trace := &streamTrace{}
	buf := make([]byte, 32*1024)
	readErr := error(nil)
	for {
		n, er := resp.Body.Read(buf)
		if n > 0 {
			if firstByte == 0 {
				firstByte = time.Since(start).Milliseconds()
			}
			w.Write(buf[:n])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			trace.Feed(buf[:n], time.Now())
			if out.Len() < 64<<20 {
				remain := 64<<20 - out.Len()
				if n < remain {
					remain = n
				}
				out.Write(buf[:remain])
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			readErr = er
			break
		}
	}
	trace.Finish(time.Now())
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	et, em := "", ""
	if !success {
		et, em = errorInfo(out.Bytes(), resp.StatusCode)
	}
	ttft := firstByte
	if trace.seen {
		ttft = trace.first.Sub(start).Milliseconds()
	}
	if success && (stream || trace.sse) && !trace.end {
		success = false
		et = "invalid_response"
		em = "upstream stream ended before a terminal event"
	}
	if success && trace.fail {
		success = false
		et = "invalid_response"
		em = "upstream reported an incomplete response"
	}
	if r.Context().Err() != nil {
		success = false
		resp.StatusCode = 499
		et = "client_cancelled"
		em = ""
	} else if readErr != nil && success {
		success = false
		et = "invalid_response"
		em = readErr.Error()
	}
	h.record(r, t, u.Path, model, stream, resp.StatusCode, success, et, em, start, ttft, body, out.Bytes(), resp.Header)
}

func contentType(h http.Header) string {
	if h == nil {
		return ""
	}
	return h.Get("Content-Type")
}

func (h *Handler) record(r *http.Request, t Target, path, model string, stream bool, status int, success bool, etype, emsg string, start time.Time, ttft int64, req, res []byte, headers http.Header) {
	pu := adapterUsage(t.Kind, stream, contentType(headers), res)
	in, out, cache, write, reason, served := pu.Input, pu.Output, pu.CacheRead, pu.CacheWrite, pu.Reasoning, pu.Model
	var ip, op, cp, wp, rp *int64
	if in+out+cache+write+reason > 0 {
		ip, op, cp, wp, rp = &in, &out, &cache, &write, &reason
	}
	var co, ci, cc, oo *float64
	var cm *int64
	var pc *float64
	known := false
	if ip != nil {
		if bd, ctx, pct, ok := pricing.CostBreakdown(first(served, model), in, out, cache); ok {
			known = true
			total, uncached, cached, completion := bd.Total, bd.InputUncached, bd.CacheRead, bd.Output
			co, ci, cc, oo = &total, &uncached, &cached, &completion
			cm, pc = &ctx, &pct
		}
	}
	if pu.CostUSD != nil { // a provider-reported cost beats the price file
		known = true
		co = pu.CostUSD
	}
	sid, fp := h.Sessions.ID(r.Header.Get("X-Llmtap-Session"), r.Header.Get("Authorization"), t.Name, time.Now())
	if t.Fingerprint != "" {
		fp = t.Fingerprint
	}
	attempt := 1
	if n, e := strconv.Atoi(r.Header.Get("X-Llmtap-Attempt")); e == nil {
		attempt = n
	}
	trace := sanitize(r.Header.Get("X-Llmtap-Trace"))
	if trace == "" {
		trace = sanitize(traceParent(r.Header.Get("traceparent")))
	}
	capture := h.C.Capture
	project := sanitize(r.Header.Get("X-Llmtap-Project"))
	if t.IntegrationID != "" {
		project = sanitize(t.ProjectLabel)
		switch t.CaptureMode {
		case "none":
			capture.Bodies = "none"
		case "truncated":
			capture.Bodies = "truncated"
		}
	}
	errorMessage := redact(emsg)
	requestPreview := preview(req, capture)
	responsePreview := preview(res, capture)
	if t.IntegrationID != "" && capture.Bodies == "none" {
		errorMessage, requestPreview, responsePreview = "", "", ""
	}
	requestBytes := int64(len(req))
	if t.RequestBytes > requestBytes {
		requestBytes = t.RequestBytes
	}
	accepted := h.S.Record(store.CallRecord{TS: start.UnixMilli(), Source: "proxy", Provider: t.Name,
		Endpoint: strings.TrimPrefix(path, "/"), ModelRequested: model, ModelServed: served, Stream: stream,
		Status: status, Success: success, ErrorType: etype, ErrorMessage: errorMessage,
		FinishReason: pu.FinishReason, Latency: time.Since(start).Milliseconds(), TTFT: ttft,
		Input: ip, Output: op, CacheRead: cp, CacheWrite: wp, Reasoning: rp, Cost: co,
		CostInput: ci, CostCache: cc, CostOutput: oo, PricingMissing: !known, ContextMax: cm,
		ContextPct: pc, SessionID: sid, Fingerprint: fp, Project: project, TraceID: trace,
		Attempt: attempt, RequestID: sanitize(headers.Get("x-request-id")),
		RequestPreview: requestPreview, ResponsePreview: responsePreview,
		RequestBytes: requestBytes, ResponseBytes: int64(len(res)),
		IntegrationID: t.IntegrationID, AgentType: t.AgentType})
	if accepted && t.IntegrationID != "" && h.Gateway != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := h.Gateway.MarkTraffic(ctx, t.IntegrationID); err != nil {
			fmt.Fprintln(os.Stderr, "llmtap integration traffic:", err)
		}
		cancel()
	}
}
func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
func hop(k string) bool {
	k = strings.ToLower(k)
	return k == "connection" || k == "keep-alive" || k == "transfer-encoding" || k == "upgrade"
}
func privateHeader(k string) bool { k = strings.ToLower(k); return strings.HasPrefix(k, "x-llmtap-") }
func sanitize(s string) string {
	if len(s) > 64 {
		return ""
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:/ -", r)) {
			return ""
		}
	}
	return s
}
func traceParent(s string) string {
	p := strings.Split(s, "-")
	if len(p) >= 2 {
		return p[1]
	}
	return ""
}
func requestInfo(b []byte) (string, bool) {
	var v struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	json.Unmarshal(b, &v)
	return v.Model, v.Stream
}
func injectUsage(b []byte) []byte {
	var v map[string]any
	if json.Unmarshal(b, &v) != nil {
		return b
	}
	if _, ok := v["stream_options"]; !ok {
		v["stream_options"] = map[string]any{"include_usage": true}
		x, _ := json.Marshal(v)
		return x
	}
	return b
}
func usage(b []byte) (int64, int64, int64, int64, string) {
	var v struct {
		Model string `json:"model"`
		Usage struct {
			Prompt        int64 `json:"prompt_tokens"`
			Completion    int64 `json:"completion_tokens"`
			Input         int64 `json:"input_tokens"`
			Output        int64 `json:"output_tokens"`
			PromptDetails struct {
				Cached int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			InputDetails struct {
				Cached int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			CompletionDetails struct {
				Reason int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
			OutputDetails struct {
				Reason int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	json.Unmarshal(b, &v)
	i, o := v.Usage.Prompt, v.Usage.Completion
	if i == 0 {
		i = v.Usage.Input
	}
	if o == 0 {
		o = v.Usage.Output
	}
	c := v.Usage.PromptDetails.Cached
	if c == 0 {
		c = v.Usage.InputDetails.Cached
	}
	rr := v.Usage.CompletionDetails.Reason
	if rr == 0 {
		rr = v.Usage.OutputDetails.Reason
	}
	return i, o, c, rr, v.Model
}
func errorInfo(b []byte, status int) (string, string) {
	var v struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(b, &v)
	t := v.Error.Type
	if t == "" {
		t = v.Error.Code
	}
	if t == "" {
		t = fmt.Sprintf("http_%d", status)
	}
	return t, truncateText(v.Error.Message, 512)
}
func redact(s string) string { return secret.ReplaceAllString(s, "[REDACTED]") }
func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
func preview(b []byte, c config.Capture) string {
	if c.Bodies == "none" {
		return ""
	}
	if len(b) > c.MaxBodyBytes && c.Bodies == "truncated" {
		b = b[:c.MaxBodyBytes]
	}
	return redact(string(b))
}
