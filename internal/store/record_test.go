package store

import (
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestRecordOverflowIsVisibleAndNotSilent(t *testing.T) {
	s := &Store{q: make(chan CallRecord, 1)}
	if !s.Record(CallRecord{TS: 1}) {
		t.Fatal("first record should be accepted")
	}
	var logs atomic.Int64
	s.dropLog = func(string) { logs.Add(1) }
	if s.Record(CallRecord{TS: 2}) {
		t.Fatal("full queue should reject the second record")
	}
	if got := s.Dropped(); got != 1 {
		t.Fatalf("dropped=%d, want 1", got)
	}
	if got := logs.Load(); got != 1 {
		t.Fatalf("overflow log calls=%d, want 1", got)
	}
}

func TestViewTreatsEmptyModelsAsUnknown(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.RecordOK(CallRecord{TS: 1, Source: "proxy", Provider: "mock", Endpoint: "v1/chat/completions",
		Status: 200, Success: true, Latency: 1}) {
		t.Fatal("record rejected")
	}
	s.Close()
	ro, err := OpenReadOnly(filepath.Join(dir, "llmtap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var model string
	if err = ro.QueryRow("SELECT model FROM calls_v").Scan(&model); err != nil {
		t.Fatal(err)
	}
	if model != "unknown" {
		t.Fatalf("empty model rendered as %q, want unknown", model)
	}
}
