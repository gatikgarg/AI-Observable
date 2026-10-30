package collector

import (
	"context"
	"testing"
	"time"

	"github.com/ai-observable/ai-observable/internal/provider"
)

type jobView struct {
	status string
	cursor *int64
	next   *int64
	run    *int64
}

func TestRunNowImportsUsageIdempotentlyAndAdvancesCursor(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()

	res, err := r.c.RunNow(ctx, "acct", "usage", false)
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if len(res) != 1 || res[0].Status != "ok" || res[0].Rows != 2 || res[0].Requests != 7 {
		t.Fatalf("unexpected result: %+v", res)
	}
	var rows, requests int
	if err := r.st.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(requests),0)
		FROM provider_usage WHERE provider='acct'`).Scan(&rows, &requests); err != nil {
		t.Fatalf("query provider_usage: %v", err)
	}
	if rows != 2 || requests != 7 {
		t.Fatalf("provider_usage rows=%d requests=%d, want 2/7", rows, requests)
	}
	imports, err := r.st.ImportRows(ctx, 10)
	if err != nil {
		t.Fatalf("ImportRows: %v", err)
	}
	if len(imports) != 1 {
		t.Fatalf("import ledger has %d rows, want 1", len(imports))
	}

	// A second full read of the same window must not duplicate rows.
	if res, err = r.c.RunNow(ctx, "acct", "usage", true); err != nil {
		t.Fatalf("second RunNow: %v", err)
	}
	if res[0].Status != "ok" {
		t.Fatalf("second run status: %+v", res[0])
	}
	if err := r.st.DB.QueryRow(`SELECT COUNT(*) FROM provider_usage WHERE provider='acct'`).Scan(&rows); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if rows != 2 {
		t.Fatalf("re-import produced %d rows, want 2", rows)
	}
	if imports, err = r.st.ImportRows(ctx, 10); err != nil {
		t.Fatalf("ImportRows: %v", err)
	}
	if len(imports) != 1 {
		t.Fatalf("re-run appended a ledger row: %d", len(imports))
	}

	jobs, err := r.db.ListJobs(ctx, r.p.ID)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	var usage jobView
	for _, j := range jobs {
		if j.Kind == "usage" {
			usage = jobView{status: j.LastStatus, cursor: j.CursorMS, next: j.NextRunAtMS, run: j.LastRunAtMS}
		}
	}
	if usage.status != "ok" || usage.cursor == nil || usage.run == nil || usage.next == nil {
		t.Fatalf("usage job not updated: %+v", usage)
	}
	if want := testNow.Truncate(time.Hour).UnixMilli(); *usage.cursor != want {
		t.Fatalf("cursor = %d, want %d", *usage.cursor, want)
	}
	if *usage.next != testNow.Add(time.Hour).UnixMilli() {
		t.Fatalf("next_run = %d, want %d", *usage.next, testNow.Add(time.Hour).UnixMilli())
	}
}

func TestAuthFailureIsNotRetriedAndMarksCredentialInvalid(t *testing.T) {
	r := newRig(t)
	r.f.err = &provider.HTTPError{Status: 401, Message: "bad key"}
	var sleeps int
	r.c.sleep = func(context.Context, time.Duration) error { sleeps++; return nil }

	res, err := r.c.RunNow(context.Background(), "acct", "usage", false)
	if err == nil {
		t.Fatal("RunNow should report the lane error")
	}
	if res[0].Status != "error" || res[0].Err == "" {
		t.Fatalf("result: %+v", res[0])
	}
	if got := r.f.usageCalls.Load(); got != 1 {
		t.Fatalf("adapter called %d times, want 1 (401 is not retryable)", got)
	}
	if sleeps != 0 {
		t.Fatalf("slept %d times, want 0", sleeps)
	}
	cred, err := r.db.GetCredential(context.Background(), r.cred.ID)
	if err != nil {
		t.Fatalf("GetCredential: %v", err)
	}
	if cred.Status != "invalid" || cred.LastError == "" {
		t.Fatalf("credential not marked invalid: %+v", cred)
	}
	jobs, _ := r.db.ListJobs(context.Background(), r.p.ID)
	for _, j := range jobs {
		if j.Kind == "usage" && (j.LastStatus != "error" || j.ConsecutiveFailures != 1) {
			t.Fatalf("usage job after failure: %+v", j)
		}
	}
}

func TestServerErrorRetriesThreeTimesThenFails(t *testing.T) {
	r := newRig(t)
	r.f.err = &provider.HTTPError{Status: 503}
	var sleeps int
	r.c.sleep = func(context.Context, time.Duration) error { sleeps++; return nil }

	if _, err := r.c.RunNow(context.Background(), "acct", "usage", false); err == nil {
		t.Fatal("RunNow should fail on 503")
	}
	if got := r.f.usageCalls.Load(); got != 4 {
		t.Fatalf("adapter called %d times, want 4 (initial + 3 retries)", got)
	}
	if sleeps != 3 {
		t.Fatalf("slept %d times, want 3", sleeps)
	}
}

func TestUnsupportedLaneIsReportedWithoutCallingTheAdapter(t *testing.T) {
	r := newRig(t)
	r.f.caps = provider.Capabilities{Models: true}
	if _, err := r.c.RunNow(context.Background(), "acct", "usage", false); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if got := r.f.usageCalls.Load(); got != 0 {
		t.Fatalf("adapter called %d times for an unsupported lane", got)
	}
	jobs, _ := r.db.ListJobs(context.Background(), r.p.ID)
	for _, j := range jobs {
		if j.Kind == "usage" && j.LastStatus != "unsupported" {
			t.Fatalf("usage job status = %q, want unsupported", j.LastStatus)
		}
	}
}
