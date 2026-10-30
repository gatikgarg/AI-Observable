package collector

import (
	"context"
	"testing"
	"time"

	"github.com/ai-observable/ai-observable/internal/control"
	"github.com/ai-observable/ai-observable/internal/store"
)

func TestReconcileWritesOnlyTheResidual(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	p := r.p
	p.ReconcileMode = "gap_fill"
	if _, err := r.db.UpdateProvider(ctx, p); err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}
	// One proxied call already counted for m1; the provider says 5 requests.
	hour := testNow.Truncate(time.Hour).Add(-2 * time.Hour)
	if !r.st.RecordOK(store.CallRecord{
		TS: hour.Add(10 * time.Minute).UnixMilli(), Source: "proxy", Provider: "acct",
		Endpoint: "/v1/chat/completions", ModelRequested: "m1", ModelServed: "m1",
		Status: 200, Success: true, TraceID: "trace-1",
	}) {
		t.Fatal("RecordOK rejected the call")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err := r.st.DB.QueryRow(`SELECT COUNT(*) FROM calls WHERE trace_id='trace-1'`).Scan(&n); err != nil {
			t.Fatalf("count calls: %v", err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("proxied call was not written")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := r.c.RunNow(ctx, "acct", "usage", true); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	var synthetic, tokens int
	if err := r.st.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(input_tokens),0) FROM calls
		WHERE provider='acct' AND trace_id LIKE 'reconcile:%'`).Scan(&synthetic, &tokens); err != nil {
		t.Fatalf("count synthetic: %v", err)
	}
	if synthetic != 6 {
		t.Fatalf("synthetic residual rows = %d, want 6 (m1 5-1, m2 2-0)", synthetic)
	}
	// The whole calls side must never exceed what the provider reported, per
	// model: m1 had 5 requests (1 proxied + 4 synthetic), m2 had 2.
	for model, want := range map[string]int{"m1": 5, "m2": 2} {
		var got int
		if err := r.st.DB.QueryRow(`SELECT COUNT(*) FROM calls WHERE provider='acct'
			AND COALESCE(NULLIF(model_served,''),model_requested) = ?`, model).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", model, err)
		}
		if got != want {
			t.Fatalf("%s calls = %d, provider reported %d requests", model, got, want)
		}
	}
}

func TestReconcileOffWritesNothing(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	hour := testNow.Truncate(time.Hour).Add(-2 * time.Hour)
	r.st.RecordOK(store.CallRecord{
		TS: hour.Add(5 * time.Minute).UnixMilli(), Source: "proxy", Provider: "acct",
		ModelServed: "m1", Status: 200, Success: true, TraceID: "trace-off",
	})
	time.Sleep(400 * time.Millisecond)
	if _, err := r.c.RunNow(ctx, "acct", "usage", true); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	var synthetic int
	if err := r.st.DB.QueryRow(`SELECT COUNT(*) FROM calls
		WHERE provider='acct' AND trace_id LIKE 'reconcile:%'`).Scan(&synthetic); err != nil {
		t.Fatalf("count synthetic: %v", err)
	}
	if synthetic != 0 {
		t.Fatalf("reconcile_mode=off wrote %d synthetic rows", synthetic)
	}
}

func TestWindowHonoursCursorAndBackfill(t *testing.T) {
	r := newRig(t)
	cursor := testNow.Add(-90 * time.Minute).Truncate(time.Hour).UnixMilli()
	j := control.Job{Kind: "usage", IntervalSeconds: 3600, CursorMS: &cursor}
	w := r.c.window(j, false, testNow)
	if want := time.UnixMilli(cursor).UTC(); !w.Start.Equal(want) {
		t.Fatalf("start = %s, want cursor %s", w.Start, want)
	}
	if want := testNow.Truncate(time.Hour); !w.End.Equal(want) {
		t.Fatalf("end = %s, want %s", w.End, want)
	}
	backfill := testNow.Truncate(time.Hour).AddDate(0, 0, -7)
	if full := r.c.window(j, true, testNow); !full.Start.Equal(backfill) {
		t.Fatalf("full start = %s, want backfill %s", full.Start, backfill)
	}
	cost := r.c.window(control.Job{Kind: "costs", IntervalSeconds: 3600}, true, testNow)
	if want := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC); !cost.End.Equal(want) {
		t.Fatalf("cost end = %s, want midnight UTC %s", cost.End, want)
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	r := newRig(t)
	// The interval is the floor: a failing lane never polls faster than a
	// healthy one. 60 s interval → first failure waits 60 s ±20 %.
	first := r.c.backoffNext(testNow, 60, 1)
	if d := first.Sub(testNow); d < 48*time.Second || d > 72*time.Second {
		t.Fatalf("first backoff = %s, want 60s ±20%%", d)
	}
	// Enough failures to reach the one-hour cap.
	late := r.c.backoffNext(testNow, 60, 8)
	if d := late.Sub(testNow); d < 48*time.Minute || d > 72*time.Minute {
		t.Fatalf("late backoff = %s, want the 1h cap ±20%%", d)
	}
	if later := r.c.backoffNext(testNow, 60, 20); later.Sub(late) > 30*time.Minute {
		t.Fatalf("backoff is not capped: %s vs %s", later.Sub(testNow), late.Sub(testNow))
	}
}

func TestDispatchSkipsDisabledJobs(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	p := r.p
	p.Enabled = false
	if _, err := r.db.UpdateProvider(ctx, p); err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}
	r.c.Dispatch(ctx)
	time.Sleep(100 * time.Millisecond)
	if got := r.f.usageCalls.Load(); got != 0 {
		t.Fatalf("disabled provider ran %d lanes", got)
	}
	jobs, _ := r.db.ListJobs(ctx, r.p.ID)
	for _, j := range jobs {
		if j.LastRunAtMS != nil {
			t.Fatalf("disabled lane %s ran at %d", j.Kind, *j.LastRunAtMS)
		}
	}
}
