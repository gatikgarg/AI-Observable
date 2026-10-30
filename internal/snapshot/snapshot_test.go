package snapshot

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ai-observable/ai-observable/internal/store"
)

func open(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if !s.RecordOK(store.CallRecord{TS: time.Now().UnixMilli(), Source: "proxy",
		Provider: "openai", Endpoint: "v1/chat/completions", ModelServed: "gpt-4o-mini",
		Status: 200, Success: true, Latency: 12}) {
		t.Fatal("record rejected")
	}
	// The async writer flushes on a 250 ms tick; snapshots in production are
	// taken by a ticker, so wait for the row here.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM calls").Scan(&n); err == nil && n == 1 {
			return s, dir
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("record was not persisted")
	return nil, ""
}

func TestTakePublishesReadableView(t *testing.T) {
	s, dir := open(t)
	snap := &Snapshotter{DB: s.DB, ViewDir: filepath.Join(dir, "view"), retry: []time.Duration{time.Millisecond}}
	if err := snap.Take(); err != nil {
		t.Fatal(err)
	}
	if snap.Last.IsZero() || snap.LastErr != nil {
		t.Fatalf("state: last=%v err=%v", snap.Last, snap.LastErr)
	}
	view := filepath.Join(dir, "view", "llmtap_view.db")
	if _, err := os.Stat(view); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(view); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("view stat = %v, %v", fi, err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(view)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int64
	var mode string
	if err = db.QueryRow("SELECT COUNT(*) FROM calls").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if n != 1 || mode != "delete" {
		t.Fatalf("view rows=%d journal=%q", n, mode)
	}
	// The view must not carry the WAL sidecar Grafana cannot read.
	if _, err := os.Stat(view + "-wal"); err == nil {
		t.Fatal("view still has a -wal file")
	}
}

func TestTakeHonoursExplicitPath(t *testing.T) {
	s, dir := open(t)
	out := filepath.Join(dir, "nested", "out.db")
	snap := &Snapshotter{DB: s.DB, ViewDir: dir, Path: out, retry: []time.Duration{time.Millisecond}}
	if err := snap.Take(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "llmtap_view.db")); err == nil {
		t.Fatal("ViewDir default was written despite Path")
	}
}

func TestConcurrentTakesDoNotCollide(t *testing.T) {
	s, dir := open(t)
	snap := &Snapshotter{DB: s.DB, ViewDir: filepath.Join(dir, "view"), retry: []time.Duration{time.Millisecond}}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = snap.Take()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("take %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, "view"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "llmtap_view.db" {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestFailureKeepsPreviousView(t *testing.T) {
	s, dir := open(t)
	view := filepath.Join(dir, "view")
	snap := &Snapshotter{DB: s.DB, ViewDir: view, retry: []time.Duration{time.Millisecond}}
	if err := snap.Take(); err != nil {
		t.Fatal(err)
	}
	good := snap.Last
	// A directory in the destination slot makes both rename and copy fail.
	dst := filepath.Join(view, "llmtap_view.db")
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dst, 0755); err != nil {
		t.Fatal(err)
	}
	if err := snap.Take(); err == nil {
		t.Fatal("expected a publish error")
	}
	if snap.LastErr == nil || !snap.Last.Equal(good) {
		t.Fatalf("failure must not advance Last: last=%v err=%v", snap.Last, snap.LastErr)
	}
}
