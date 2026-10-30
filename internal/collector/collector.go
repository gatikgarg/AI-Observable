// Package collector implements lane B (pull reconciliation) and lane C
// (catalog/prices). It never runs unless a provider row enables a lane, never
// touches calls except through the gap_fill residual path, and never retries an
// authentication failure.
package collector

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ai-observable/ai-observable/internal/config"
	"github.com/ai-observable/ai-observable/internal/control"
	"github.com/ai-observable/ai-observable/internal/store"
	"github.com/ai-observable/ai-observable/internal/vault"
)

// jobTimeout bounds one lane run end to end (plan §7.2).
const jobTimeout = 60 * time.Second

// Result is what one lane run did, for the admin API and the CLI.
type Result struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	Status   string `json:"status"`
	Rows     int    `json:"rows_imported"`
	Requests int64  `json:"requests_imported"`
	Err      string `json:"error,omitempty"`
}

// Collector owns the pull scheduler.
type Collector struct {
	DB     *control.DB
	Store  *store.Store
	Vault  *vault.Vault
	Config config.Config
	Client *http.Client

	// Snapshot is invoked (coalesced) after an import that changed rows.
	Snapshot func()

	sem      chan struct{}    // total in-flight lanes
	slots    sync.Map         // provider id -> chan struct{} (one lane per provider)
	now      func() time.Time // injectable clock
	sleep    func(context.Context, time.Duration) error
	snapMu   sync.Mutex
	lastSnap time.Time
	// PollInterval is how often the persisted schedule is inspected.
	PollInterval time.Duration
}

// New builds a collector. Nothing starts until Run or RunNow is called.
func New(db *control.DB, st *store.Store, v *vault.Vault, cfg config.Config) *Collector {
	n := cfg.Collector.MaxConcurrent
	if n < 1 {
		n = 3
	}
	c := &Collector{
		DB: db, Store: st, Vault: v, Config: cfg,
		Client: &http.Client{
			// The total budget stays under the 60 s job budget.
			Timeout: 55 * time.Second,
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				ResponseHeaderTimeout: 15 * time.Second,
				MaxIdleConnsPerHost:   2,
			},
		},
		sem:          make(chan struct{}, n),
		now:          func() time.Time { return time.Now().UTC() },
		sleep:        sleepCtx,
		PollInterval: 15 * time.Second,
	}
	wireReconcileMode(db, st)
	return c
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}


// Run polls collector_jobs until ctx is cancelled. Due jobs are dispatched on
// their own goroutines, bounded by max_concurrent and one lane per provider.
func (c *Collector) Run(ctx context.Context) {
	every := c.PollInterval
	if every <= 0 {
		every = 15 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		c.Dispatch(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Dispatch starts every due lane it has capacity for. Lanes that do not fit
// stay due, so the next poll picks them up instead of piling up goroutines.
func (c *Collector) Dispatch(ctx context.Context) {
	jobs, err := c.DB.DueJobs(ctx, c.now().UnixMilli())
	if err != nil {
		return
	}
	for _, j := range jobs {
		slot := c.providerSlot(j.ProviderID)
		select {
		case slot <- struct{}{}:
		default:
			continue // one lane per provider at a time
		}
		select {
		case c.sem <- struct{}{}:
			go func(j control.Job) {
				defer func() { <-c.sem; <-slot }()
				_, _ = c.RunJob(ctx, j)
			}(j)
		default:
			<-slot
			return // global capacity reached; retry on the next poll
		}
	}
}

func (c *Collector) providerSlot(providerID string) chan struct{} {
	if v, ok := c.slots.Load(providerID); ok {
		return v.(chan struct{})
	}
	v, _ := c.slots.LoadOrStore(providerID, make(chan struct{}, 1))
	return v.(chan struct{})
}

// RunNow runs lanes on demand: providerRef is an id or name ("" = every
// provider), kind is a lane name ("" = every lane), full re-reads the whole
// backfill window instead of the cursor.
func (c *Collector) RunNow(ctx context.Context, providerRef, kind string, full bool) ([]Result, error) {
	providers, err := c.providers(ctx, providerRef)
	if err != nil {
		return nil, err
	}
	if kind != "" && !validKind(kind) {
		return nil, fmt.Errorf("collector: kind %q must be one of %v", kind, control.JobKinds)
	}
	var out []Result
	var firstErr error
	for _, p := range providers {
		jobs, err := c.DB.ListJobs(ctx, p.ID)
		if err != nil {
			return out, err
		}
		for _, job := range jobs {
			if kind != "" && job.Kind != kind {
				continue
			}
			job.IntervalSeconds = c.interval(job.Kind)
			r := c.run(ctx, job, full)
			out = append(out, r)
			if r.Status == "error" && firstErr == nil {
				firstErr = fmt.Errorf("collector: %s/%s: %s", r.Provider, r.Kind, r.Err)
			}
		}
	}
	return out, firstErr
}

func (c *Collector) providers(ctx context.Context, ref string) ([]control.Provider, error) {
	if ref != "" {
		p, err := c.DB.GetProvider(ctx, ref)
		if err != nil {
			return nil, err
		}
		return []control.Provider{p}, nil
	}
	return c.DB.ListProviders(ctx)
}

func validKind(kind string) bool {
	for _, k := range control.JobKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// interval resolves the configured default for a lane.
func (c *Collector) interval(kind string) int {
	cc := c.Config.Collector
	var v int
	switch kind {
	case "usage":
		v = cc.UsageIntervalSeconds
	case "costs":
		v = cc.CostsIntervalSeconds
	case "models":
		v = cc.ModelsIntervalSeconds
	case "balance":
		v = cc.BalanceIntervalSeconds
	}
	if v <= 0 {
		v = 3600
	}
	return v
}

// RunJob executes one persisted lane, updating collector_jobs and the audit
// log. The returned error is informational: the row already carries it.
func (c *Collector) RunJob(ctx context.Context, j control.Job) (Result, error) {
	r := c.run(ctx, j, j.CursorMS == nil && j.LastRunAtMS == nil)
	if r.Status == "error" {
		return r, fmt.Errorf("collector: %s/%s: %s", r.Provider, r.Kind, r.Err)
	}
	return r, nil
}

// requestSnapshot coalesces snapshot requests to at most one per 2 s.
func (c *Collector) requestSnapshot() {
	if c.Snapshot == nil {
		return
	}
	c.snapMu.Lock()
	if time.Since(c.lastSnap) < 2*time.Second {
		c.snapMu.Unlock()
		return
	}
	c.lastSnap = time.Now()
	c.snapMu.Unlock()
	c.Snapshot()
}
