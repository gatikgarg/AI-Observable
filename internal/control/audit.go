package control

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// AuditEntry is one row of the audit log, oldest last.
type AuditEntry struct {
	ID     int64  `json:"id"`
	TS     int64  `json:"ts_ms"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// secretKeyPattern matches detail keys whose value must never be stored.
var secretKeyPattern = regexp.MustCompile(`(?i)(secret|password|token|api[_-]?key|authorization|credential|bearer)`)

// secretValuePatterns catch key-shaped substrings anywhere in a detail string,
// so "added key sk-live-…" cannot smuggle a secret into the audit log.
var secretValuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{6,}`),
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{6,}`),
	regexp.MustCompile(`gsk_[A-Za-z0-9_-]{6,}`),
	regexp.MustCompile(`or-[A-Za-z0-9_-]{6,}`),
	regexp.MustCompile(`AIza[A-Za-z0-9_-]{6,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._-]{6,}`),
}

// Audit appends one audit row. detail is marshalled to JSON and scrubbed: keys
// that look secret are redacted and key-shaped values are removed from strings.
func (d *DB) Audit(ctx context.Context, actor, action, target string, detail any) error {
	payload, err := marshalDetail(detail)
	if err != nil {
		return invalid("audit detail: %v", err)
	}
	_, err = d.db.ExecContext(ctx,
		"INSERT INTO audit(ts_ms,actor,action,target,detail) VALUES(?,?,?,?,?)",
		nowMS(), actor, action, target, payload)
	if err != nil {
		return wrap("audit", err)
	}
	return nil
}

func marshalDetail(detail any) (string, error) {
	if detail == nil {
		return "", nil
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return "", err
	}
	var generic any
	if err = json.Unmarshal(raw, &generic); err != nil {
		return scrubText(string(raw)), nil
	}
	scrubbed, err := json.Marshal(scrubAny(generic))
	if err != nil {
		return "", err
	}
	return string(scrubbed), nil
}

func scrubAny(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if secretKeyPattern.MatchString(k) {
				out[k] = "[redacted]"
				continue
			}
			out[k] = scrubAny(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = scrubAny(val)
		}
		return out
	case string:
		return scrubText(t)
	default:
		return v
	}
}

// scrubText replaces anything shaped like an API key with "…".
func scrubText(s string) string {
	for _, re := range secretValuePatterns {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return strings.TrimSpace(s)
}

// ScrubText is exported for the log scrubber and response previews so there is
// exactly one definition of "looks like a secret".
func ScrubText(s string) string { return scrubText(s) }

// AuditEntries returns the newest limit rows, newest first. A non-positive
// limit means 100.
func (d *DB) AuditEntries(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.db.QueryContext(ctx, `SELECT id,ts_ms,COALESCE(actor,''),action,
		COALESCE(target,''),COALESCE(detail,'') FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, wrap("list audit", err)
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if serr := rows.Scan(&e.ID, &e.TS, &e.Actor, &e.Action, &e.Target, &e.Detail); serr != nil {
			return nil, wrap("list audit", serr)
		}
		out = append(out, e)
	}
	if err = rows.Err(); err != nil {
		return nil, wrap("list audit", err)
	}
	if out == nil {
		out = []AuditEntry{}
	}
	return out, nil
}

// CountAudit is used by the diagnostics bundle.
func (d *DB) CountAudit(ctx context.Context) (int, error) {
	var n int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit").Scan(&n); err != nil {
		return 0, wrap("count audit", err)
	}
	return n, nil
}
