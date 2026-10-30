package admin

import (
	"sync"
	"time"

	"github.com/ai-observable/ai-observable/internal/control"
)

func nowMS() int64 { return time.Now().UnixMilli() }

// Entry is one scrubbed log line, kept in memory only.
type Entry struct {
	TimeMS  int64    `json:"ts_ms"`
	Level   string   `json:"level"`
	Message string   `json:"message"`
	Fields  []string `json:"fields,omitempty"`
}

// Ring is a fixed-size in-memory log buffer. It exists so the UI can show what
// the last request did without reading a log file, and it never carries a
// secret: every message goes through control.ScrubText.
type Ring struct {
	mu    sync.Mutex
	buf   []Entry
	next  int
	count int
}

// NewRing allocates a ring holding at most n entries.
func NewRing(n int) *Ring {
	if n < 1 {
		n = 200
	}
	return &Ring{buf: make([]Entry, n)}
}

// Add appends a line. Fields are alternating key/value strings.
func (r *Ring) Add(level, message string, fields ...string) {
	if r == nil {
		return
	}
	e := Entry{Level: level, Message: control.ScrubText(message)}
	for i := 0; i+1 < len(fields); i += 2 {
		e.Fields = append(e.Fields, fields[i]+"="+control.ScrubText(fields[i+1]))
	}
	r.mu.Lock()
	e.TimeMS = nowMS()
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
	if r.count < len(r.buf) {
		r.count++
	}
	r.mu.Unlock()
}

// List returns the newest entries first, optionally filtered by level.
func (r *Ring) List(level string, limit int) []Entry {
	if r == nil {
		return []Entry{}
	}
	if limit <= 0 || limit > len(r.buf) {
		limit = len(r.buf)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, 0, limit)
	for i := 1; i <= r.count && len(out) < limit; i++ {
		idx := (r.next - i + len(r.buf)*2) % len(r.buf)
		e := r.buf[idx]
		if level != "" && e.Level != level {
			continue
		}
		out = append(out, e)
	}
	return out
}
