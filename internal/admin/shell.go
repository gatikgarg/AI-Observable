package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"html/template"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/ai-observable/ai-observable/internal/webui"
)

// shellData is what the embedded page needs at render time. Nothing secret
// beyond the per-process CSRF token ever reaches the template.
type shellData struct {
	CSRF    string
	Version string
	Grafana string
	CSSHash string
	JSHash  string
	SVGHash string
}

// shellCSP is restrictive but still allows the small inline style attributes
// and the wizard's inline click handler the shell ships with.
const shellCSP = "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; " +
	"script-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; " +
	"base-uri 'none'; form-action 'self'"

var (
	shellOnce sync.Once
	shellTmpl *template.Template
	shellErr  error
)

func shellTemplate() (*template.Template, error) {
	shellOnce.Do(func() {
		shellTmpl, shellErr = template.New("shell").Parse(string(webui.Shell()))
	})
	return shellTmpl, shellErr
}

// assetHash is the short content hash the shell puts in a URL so
// Cache-Control: immutable stays honest.
func assetHash(name string) string {
	if _, sum, ok := webui.Asset(name); ok {
		return sum
	}
	return "dev"
}

// shell serves the manager page. It is never cached: the CSRF token changes
// with the process.
func (s *Server) shell(w http.ResponseWriter, r *http.Request) {
	tmpl, err := shellTemplate()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "shell template: "+err.Error())
		return
	}
	version := s.Version
	if version == "" {
		version = "dev"
	}
	data := shellData{
		CSRF: s.csrf, Version: version, Grafana: s.effectiveConfig().Server.GrafanaURL,
		CSSHash: assetHash("app.css"), JSHash: assetHash("app.js"), SVGHash: assetHash("logo.svg"),
	}
	var buf bytes.Buffer
	if err = tmpl.Execute(&buf, data); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "render shell: "+err.Error())
		return
	}
	sum := sha256.Sum256(buf.Bytes())
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", shellCSP)
	h.Set("X-Frame-Options", "DENY")
	h.Set("ETag", etag)
	if strings.TrimSpace(r.Header.Get("If-None-Match")) == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// assetTypes maps the few extensions embedded with the shell. Anything else is
// served as octets rather than guessed.
var assetTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
	".json":  "application/json",
}

// asset serves one embedded file with a strong ETag and immutable caching.
func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	name := path.Base(strings.TrimSpace(r.PathValue("name")))
	body, sum, found := webui.Asset(name)
	if !found || name == "" {
		http.NotFound(w, r)
		return
	}
	ct, known := assetTypes[strings.ToLower(path.Ext(name))]
	if !known {
		ct = "application/octet-stream"
	}
	etag := `"` + sum + `"`
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", etag)
	if strings.TrimSpace(r.Header.Get("If-None-Match")) == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
