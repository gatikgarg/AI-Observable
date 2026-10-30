package collector

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-observable/ai-observable/internal/config"
	"github.com/ai-observable/ai-observable/internal/control"
	"github.com/ai-observable/ai-observable/internal/provider"
	"github.com/ai-observable/ai-observable/internal/store"
	"github.com/ai-observable/ai-observable/internal/vault"
)

// testNow is a fixed clock so windows and cursors are deterministic.
var testNow = time.Date(2026, 10, 26, 12, 34, 0, 0, time.UTC)

// fakeAdapter replaces the installed openai adapter inside this package's test
// binary. It never touches the network.
type fakeAdapter struct {
	usage      []provider.UsageBucket
	costs      []provider.CostBucket
	models     []provider.ModelInfo
	balance    *provider.Balance
	err        error
	caps       provider.Capabilities
	usageCalls atomic.Int32
}

func (f *fakeAdapter) Kind() string { return "openai" }

func (f *fakeAdapter) Capabilities() provider.Capabilities {
	if f.caps == (provider.Capabilities{}) {
		return provider.Capabilities{Models: true, Usage: true, Costs: true}
	}
	return f.caps
}

func (f *fakeAdapter) Validate(context.Context, provider.Credential, provider.Provider) (provider.Identity, error) {
	return provider.Identity{}, f.err
}

func (f *fakeAdapter) ListModels(context.Context, provider.Credential, provider.Provider) ([]provider.ModelInfo, error) {
	return f.models, f.err
}

func (f *fakeAdapter) Usage(context.Context, provider.Credential, provider.Provider, provider.Window) ([]provider.UsageBucket, error) {
	f.usageCalls.Add(1)
	return f.usage, f.err
}

func (f *fakeAdapter) Costs(context.Context, provider.Credential, provider.Provider, provider.Window) ([]provider.CostBucket, error) {
	return f.costs, f.err
}

func (f *fakeAdapter) Balance(context.Context, provider.Credential, provider.Provider) (*provider.Balance, error) {
	return f.balance, f.err
}

func (f *fakeAdapter) ParseUsage(bool, string, []byte) (provider.Usage, error) {
	return provider.Usage{}, nil
}

type rig struct {
	db   *control.DB
	st   *store.Store
	v    *vault.Vault
	c    *Collector
	f    *fakeAdapter
	p    control.Provider
	cred control.Credential
}

func standardUsage() []provider.UsageBucket {
	hour := testNow.Truncate(time.Hour).Add(-2 * time.Hour)
	return []provider.UsageBucket{
		{Model: "m1", Start: hour, End: hour.Add(time.Hour), Requests: 5, Input: 100, Output: 50},
		{Model: "m2", Start: hour, End: hour.Add(time.Hour), Requests: 2, Input: 20, Output: 10},
	}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	db, err := control.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatalf("control.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := store.Open(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)
	v, err := vault.Open(filepath.Join(dir, "keys"), "file")
	if err != nil {
		t.Fatalf("vault.Open: %v", err)
	}
	f := &fakeAdapter{
		usage: standardUsage(),
		costs: []provider.CostBucket{{
			Start: testNow.Truncate(24 * time.Hour).Add(-24 * time.Hour),
			End:   testNow.Truncate(24 * time.Hour), Amount: 1.25, Currency: "USD", LineItem: "tokens",
		}},
		models:  []provider.ModelInfo{{ID: "m1", DisplayName: "Model One", ContextMax: 128000}},
		balance: &provider.Balance{Amount: 42.5, Currency: "USD"},
	}
	provider.Register(f)
	ctx := context.Background()
	p, err := db.CreateProvider(ctx, control.Provider{
		Name: "acct", Kind: "openai", BaseURL: "https://api.openai.com", AuthMode: "managed",
	})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	cred, err := db.AddCredential(ctx, p.ID, "main", "admin", "sk-admin-secret-value", v)
	if err != nil {
		t.Fatalf("AddCredential: %v", err)
	}
	for _, kind := range control.JobKinds {
		if _, err := db.UpsertJob(ctx, control.Job{
			ProviderID: p.ID, Kind: kind, Enabled: true, IntervalSeconds: 3600,
		}); err != nil {
			t.Fatalf("UpsertJob(%s): %v", kind, err)
		}
	}
	c := New(db, st, v, config.Config{Collector: config.Collector{BackfillDays: 7, MaxConcurrent: 2}})
	c.now = func() time.Time { return testNow }
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return &rig{db: db, st: st, v: v, c: c, f: f, p: p, cred: cred}
}
