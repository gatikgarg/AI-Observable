package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

type gatewayFailures struct {
	count int
	since time.Time
}

func (h *Handler) serveGateway(w http.ResponseWriter, r *http.Request) {
	if h.Gateway == nil {
		openAIError(w, http.StatusNotFound, "observable_gateway_unavailable", "agent gateway is not running")
		return
	}
	if !loopbackHost(r.Host) || !loopbackOrigin(r.Header.Get("Origin")) {
		openAIError(w, http.StatusForbidden, "observable_origin_rejected", "gateway accepts loopback clients only")
		return
	}
	source := remoteHost(r.RemoteAddr)
	if !h.allowGatewaySource(source) {
		w.Header().Set("Retry-After", "60")
		openAIError(w, http.StatusTooManyRequests, "observable_auth_throttled", "too many invalid gateway token attempts")
		return
	}
	token := gatewayToken(r)
	if token == "" {
		h.failedGatewayAuth(source)
		openAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing Observable gateway token")
		return
	}
	binding, err := h.Gateway.Authenticate(r.Context(), token)
	if err != nil {
		h.failedGatewayAuth(source)
		openAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid or revoked Observable gateway token")
		return
	}
	h.resetGatewaySource(source)

	rest := strings.TrimPrefix(r.URL.Path, "/gw/v1/")
	if !gatewayPathAllowed(r.Method, rest) {
		openAIError(w, http.StatusNotFound, "observable_path_not_allowed", "gateway path is not allowlisted")
		return
	}
	if rest == "models" || strings.HasPrefix(rest, "models/") {
		atomic.AddInt64(&h.modelsSeen, 1)
	}
	t, err := h.resolve(r.Context(), binding.ProviderName)
	if err != nil || (t.ID != "" && binding.ProviderID != "" && t.ID != binding.ProviderID) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = h.Gateway.MarkAttention(ctx, binding.IntegrationID)
		cancel()
		openAIError(w, http.StatusServiceUnavailable, "observable_provider_unavailable", "the integration provider is unavailable")
		return
	}
	t.AuthMode = "managed"
	t.IntegrationID = binding.IntegrationID
	t.AgentType = binding.AgentType
	t.ProjectLabel = binding.Label
	t.CaptureMode = binding.CaptureMode
	u, err := gatewayUpstream(t.Upstream, rest)
	if err != nil {
		openAIError(w, http.StatusBadGateway, "observable_upstream_invalid", "the provider endpoint is invalid")
		return
	}

	body, requestReader, requestBytes, err := prepareGatewayBody(r, 64<<20)
	if err != nil {
		openAIError(w, http.StatusBadRequest, "observable_request_invalid", "could not read request body")
		return
	}
	model, stream := binding.Model, false
	if requestReader == nil {
		model, stream = requestInfo(body)
	}
	if model == "" {
		model = binding.Model
	}
	if requestReader == nil && stream && t.StreamUsage && rest == "chat/completions" {
		body = injectUsage(body)
	}
	t.RequestBytes = requestBytes
	var upstreamBody io.Reader = bytes.NewReader(body)
	if requestReader != nil {
		upstreamBody = requestReader
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), upstreamBody)
	if err != nil {
		openAIError(w, http.StatusBadGateway, "observable_request_invalid", "could not create upstream request")
		return
	}
	if requestReader == nil {
		req.ContentLength = int64(len(body))
	} else if requestBytes > 0 {
		req.ContentLength = requestBytes
	} else {
		req.ContentLength = -1
	}
	for k, v := range r.Header {
		if hop(k) || strings.EqualFold(k, "Accept-Encoding") || privateHeader(k) {
			continue
		}
		req.Header[k] = append([]string(nil), v...)
	}
	applyAuth(req, t)
	for k, v := range t.Extra {
		if k != "" && !privateHeader(k) && !hop(k) {
			req.Header.Set(k, v)
		}
	}
	start := time.Now()
	resp, err := h.Client.Do(req)
	if err != nil {
		status := 0
		etype := "connection_error"
		if r.Context().Err() != nil {
			status, etype = 499, "client_cancelled"
		} else if strings.Contains(strings.ToLower(err.Error()), "timeout") {
			etype = "timeout"
		}
		if rest != "models" && !strings.HasPrefix(rest, "models/") {
			h.record(r, t, "v1/"+rest, model, stream, status, false, etype, err.Error(), start, 0, body, nil, nil)
		}
		openAIError(w, http.StatusBadGateway, "observable_upstream_unreachable", "provider request failed")
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)

	var out bytes.Buffer
	var firstContent time.Time
	firstByte := int64(0)
	trace := &streamTrace{}
	buf := make([]byte, 32*1024)
	var readErr error
	for {
		n, er := resp.Body.Read(buf)
		if n > 0 {
			if firstByte == 0 {
				firstByte = time.Since(start).Milliseconds()
			}
			chunk := buf[:n]
			if out.Len() < 64<<20 {
				remain := min(n, 64<<20-out.Len())
				out.Write(chunk[:remain])
			}
			trace.Feed(chunk, time.Now())
			if trace.seen && firstContent.IsZero() {
				firstContent = trace.first
			}
			if _, werr := w.Write(chunk); werr != nil {
				readErr = werr
				break
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if er != nil {
			if !errors.Is(er, context.Canceled) && !errors.Is(er, io.EOF) {
				readErr = er
			}
			break
		}
	}
	trace.Finish(time.Now())
	success := resp.StatusCode >= 200 && resp.StatusCode < 300
	etype, emsg := "", ""
	if !success {
		etype, emsg = errorInfo(out.Bytes(), resp.StatusCode)
	}
	if success && (stream || trace.sse) && !trace.end {
		success, etype, emsg = false, "invalid_response", "upstream stream ended before a terminal event"
	}
	if trace.fail {
		success, etype, emsg = false, "invalid_response", "upstream reported an incomplete response"
	}
	status := resp.StatusCode
	if r.Context().Err() != nil {
		status, success, etype, emsg = 499, false, "client_cancelled", ""
	}
	if readErr != nil && success {
		success, etype, emsg = false, "invalid_response", readErr.Error()
	}
	ttft := firstByte
	if !firstContent.IsZero() {
		ttft = firstContent.Sub(start).Milliseconds()
	}
	if rest != "models" && !strings.HasPrefix(rest, "models/") {
		h.record(r, t, "v1/"+rest, model, stream, status, success, etype, emsg,
			start, ttft, body, out.Bytes(), resp.Header)
	}
}

func gatewayToken(r *http.Request) string {
	if a := strings.TrimSpace(r.Header.Get("Authorization")); a != "" {
		scheme, value, ok := strings.Cut(a, " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(value)
		}
	}
	if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
		return v
	}
	return strings.TrimSpace(r.Header.Get("api-key"))
}

func gatewayPathAllowed(method, path string) bool {
	if method == http.MethodPost {
		return path == "chat/completions" || path == "responses"
	}
	if method == http.MethodGet {
		return path == "models" || (strings.HasPrefix(path, "models/") && !strings.Contains(path, ".."))
	}
	return false
}

func gatewayUpstream(base, rest string) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid provider URL")
	}
	prefix := strings.TrimRight(u.Path, "/")
	if !hasVersionSuffix(prefix) {
		prefix += "/v1"
	}
	u.Path = prefix + "/" + rest
	return u, nil
}

func hasVersionSuffix(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 {
		return false
	}
	last := parts[len(parts)-1]
	return last == "v1" || last == "v1beta" || strings.HasPrefix(last, "v1/")
}

func prepareGatewayBody(r *http.Request, max int64) ([]byte, io.ReadCloser, int64, error) {
	if r.Body == nil {
		return nil, nil, 0, nil
	}
	if r.ContentLength > max {
		return nil, r.Body, r.ContentLength, nil
	}
	prefix, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		r.Body.Close()
		return nil, nil, 0, err
	}
	if int64(len(prefix)) > max {
		return nil, &joinedReadCloser{Reader: io.MultiReader(bytes.NewReader(prefix), r.Body), Closer: r.Body}, 0, nil
	}
	r.Body.Close()
	return prefix, nil, int64(len(prefix)), nil
}

type joinedReadCloser struct {
	io.Reader
	io.Closer
}

func remoteHost(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err == nil {
		return h
	}
	return addr
}

func loopbackHost(hostport string) bool {
	h := hostport
	if only, _, err := net.SplitHostPort(hostport); err == nil {
		h = only
	}
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func loopbackOrigin(origin string) bool {
	if strings.TrimSpace(origin) == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && loopbackHost(u.Host)
}

func (h *Handler) allowGatewaySource(source string) bool {
	h.gwMu.Lock()
	defer h.gwMu.Unlock()
	f, ok := h.gwFailures[source]
	if !ok || time.Since(f.since) > time.Minute {
		return true
	}
	return f.count < 10
}

func (h *Handler) failedGatewayAuth(source string) {
	h.gwMu.Lock()
	defer h.gwMu.Unlock()
	f := h.gwFailures[source]
	if time.Since(f.since) > time.Minute {
		f = gatewayFailures{since: time.Now()}
	}
	f.count++
	h.gwFailures[source] = f
}

func (h *Handler) resetGatewaySource(source string) {
	h.gwMu.Lock()
	delete(h.gwFailures, source)
	h.gwMu.Unlock()
}

// ModelsSeen reports allowlisted model-discovery requests, which are forwarded
// but intentionally not written to calls.
func (h *Handler) ModelsSeen() int64 { return atomic.LoadInt64(&h.modelsSeen) }

func openAIError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"message":%q,"type":"invalid_request_error","code":%q}}`, message, code)
}
