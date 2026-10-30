// Package snapshot publishes a read-only copy of the live analytics database for
// Grafana. The live DB is only read (VACUUM INTO), never modified.
package snapshot

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Snapshotter copies DB into a view file. The zero value is not usable: DB is
// required. ViewDir defaults the publish path to ViewDir/llmtap_view.db; Path
// overrides it with an explicit output filename.
type Snapshotter struct {
	DB      *sql.DB
	ViewDir string
	Path    string

	mu      sync.Mutex
	Last    time.Time
	LastErr error

	// retry is the publish backoff; tests shorten it.
	retry []time.Duration
}

var defaultRetry = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}

// Target is the file Grafana mounts.
func (s *Snapshotter) Target() string {
	if s.Path != "" {
		return s.Path
	}
	return filepath.Join(s.ViewDir, "llmtap_view.db")
}

// Take creates a consistent copy of the live DB and atomically publishes it. On
// failure the previous view is left untouched, so the dashboards keep serving
// slightly older data instead of breaking.
func (s *Snapshotter) Take() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.take(); err != nil {
		s.LastErr = err
		return err
	}
	s.Last = time.Now().UTC()
	s.LastErr = nil
	return nil
}

func (s *Snapshotter) take() error {
	dst := s.Target()
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	// A unique temp name keeps concurrent Take calls (ticker, /api/snapshot,
	// shutdown) from colliding on one file.
	tmp := filepath.Join(dir, fmt.Sprintf(".llmtap_view.%d.tmp.db", time.Now().UnixNano()))
	defer os.Remove(tmp)
	if _, err := s.DB.Exec("VACUUM INTO '" + strings.ReplaceAll(filepath.ToSlash(tmp), "'", "''") + "'"); err != nil {
		return fmt.Errorf("vacuum snapshot: %w", err)
	}
	if err := finalize(tmp); err != nil {
		return err
	}
	return s.publish(tmp, dst)
}

// finalize leaves the copy in rollback-journal mode (Grafana's SQLite plugin
// cannot read WAL files next to a read-only mount) and verifies it is readable.
func finalize(path string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=rw")
	if err != nil {
		return err
	}
	if _, err = db.Exec("PRAGMA journal_mode=DELETE"); err == nil {
		var check string
		if err = db.QueryRow("PRAGMA quick_check").Scan(&check); err == nil && check != "ok" {
			err = fmt.Errorf("snapshot integrity check: %s", check)
		}
	}
	db.Close()
	if err != nil {
		return err
	}
	return os.Chmod(path, 0644)
}

func (s *Snapshotter) publish(tmp, dst string) error {
	backoff := s.retry
	if backoff == nil {
		backoff = defaultRetry
	}
	var err error
	for attempt := 0; ; attempt++ {
		// Windows refuses to replace a file Grafana has open, so retry; the
		// current view stays valid while we wait.
		if err = os.Rename(tmp, dst); err == nil {
			return nil
		}
		if attempt >= len(backoff) {
			break
		}
		time.Sleep(backoff[attempt])
	}
	if cerr := copyOver(tmp, dst); cerr != nil {
		return fmt.Errorf("publish snapshot: %w", err)
	}
	return nil
}

// copyOver is the in-place fallback when a rename cannot happen (e.g. a locked
// destination on Windows). It only truncates the existing view once the new
// bytes are open for reading.
func copyOver(tmp, dst string) error {
	src, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, src); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
