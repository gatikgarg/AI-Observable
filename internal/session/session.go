package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

type Manager struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func New() *Manager { return &Manager{last: map[string]time.Time{}} }
func (m *Manager) ID(header, auth, provider string, now time.Time) (string, string) {
	if header != "" {
		return header, ""
	}
	h := sha256.Sum256([]byte(auth))
	fp := hex.EncodeToString(h[:])[:8]
	k := fp + ":" + provider
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.last[k]
	if p.IsZero() || now.Sub(p) > 30*time.Minute {
		m.last[k] = now
		return fp + "-" + fmtTime(now), fp
	}
	m.last[k] = now
	return fp + "-" + fmtTime(p), fp
}
func fmtTime(t time.Time) string { return string([]byte(fmt.Sprintf("%x", t.Unix()/1800))) }
