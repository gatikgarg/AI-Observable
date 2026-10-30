package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// JoinURL appends path to the provider base URL. The base URL is a prefix, not
// a full endpoint, so a gateway mounted under /openai/v1 keeps working.
func JoinURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	if path == "" {
		return base
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

// Auth applies the auth style to a request. The secret is never placed in a
// header the caller can log: query-key auth is the only case that touches the
// URL, and every error path strips the query. Provider-specific extras
// (api-version, deployment) are applied by the adapters that understand them.
func (p Provider) Auth(req *http.Request, secret string) {
	if secret != "" {
		switch p.AuthStyle {
		case "bearer":
			req.Header.Set("Authorization", "Bearer "+secret)
		case "x-api-key":
			req.Header.Set("x-api-key", secret)
		case "api-key":
			req.Header.Set("api-key", secret)
		case "x-goog-api-key":
			req.Header.Set("x-goog-api-key", secret)
		case "query-key":
			q := req.URL.Query()
			q.Set("key", secret)
			req.URL.RawQuery = q.Encode()
		case "none":
		}
	}
	req.Header.Set("User-Agent", "llmtap")
	req.Header.Set("Accept", "application/json")
}

// SafeURL drops the query and userinfo so error text can never leak a key.
func SafeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid url>"
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// Do performs one authenticated provider API call and decodes a JSON response.
// Non-2xx responses become *HTTPError carrying the provider's own message
// (scrubbed of the request URL). A 204 or an empty body decodes to nothing
// without error.
func Do(ctx context.Context, hc *http.Client, p Provider, secret, method, rawURL string, body []byte, out any) error {
	return DoHeaders(ctx, hc, p, secret, method, rawURL, nil, body, out)
}

// DoHeaders is Do with extra request headers, used by providers that require a
// version header (Anthropic).
func DoHeaders(ctx context.Context, hc *http.Client, p Provider, secret, method, rawURL string, headers map[string]string, body []byte, out any) error {
	req, err := newRequest(ctx, method, rawURL, body)
	if err != nil {
		return err
	}
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	p.Auth(req, secret)
	return send(hc, req, rawURL, out)
}

// DoJSON performs one unauthenticated JSON call. Adapters use Do.
func DoJSON(ctx context.Context, hc *http.Client, method, rawURL string, headers map[string]string, body []byte, out any) error {
	req, err := newRequest(ctx, method, rawURL, body)
	if err != nil {
		return err
	}
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	return send(hc, req, rawURL, out)
}

func newRequest(ctx context.Context, method, rawURL string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, fmt.Errorf("provider: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "llmtap")
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func send(hc *http.Client, req *http.Request, rawURL string, out any) error {
	if hc == nil {
		hc = Client()
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("provider: %s %s: %w", req.Method, SafeURL(rawURL), err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("provider: read %s: %w", SafeURL(rawURL), err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		he := &HTTPError{Status: resp.StatusCode, Message: errorMessage(raw)}
		he.Code = errorCode(raw)
		he.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		return he
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("provider: decode %s: %w", SafeURL(rawURL), err)
	}
	return nil
}

// WithQuery appends an encoded query string to rawURL.
func WithQuery(rawURL string, q url.Values) string {
	if len(q) == 0 {
		return rawURL
	}
	if strings.Contains(rawURL, "?") {
		return rawURL + "&" + q.Encode()
	}
	return rawURL + "?" + q.Encode()
}

// errorMessage extracts the human message from the common error envelopes.
func errorMessage(raw []byte) string {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal(raw, &env) == nil {
		switch {
		case env.Error.Message != "":
			return truncate(env.Error.Message)
		case env.Message != "":
			return truncate(env.Message)
		case env.Detail != "":
			return truncate(env.Detail)
		}
	}
	return truncate(strings.TrimSpace(string(raw)))
}

func errorCode(raw []byte) string {
	var env struct {
		Error struct {
			Code any    `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil {
		switch v := env.Error.Code.(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			return strconv.FormatInt(int64(v), 10)
		}
		return env.Error.Type
	}
	return ""
}

func truncate(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		d := time.Duration(n) * time.Second
		if d > time.Hour {
			d = time.Hour
		}
		return d
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d < 0 {
			return 0
		}
		if d > time.Hour {
			d = time.Hour
		}
		return d
	}
	return 0
}
