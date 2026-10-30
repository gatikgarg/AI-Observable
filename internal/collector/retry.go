package collector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/ai-observable/ai-observable/internal/provider"
)

// jitter returns a random duration in [-d, +d]. Callers add it to a base
// duration, so d is the maximum deviation (20 % of the base is the plan).
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	span := int64(d)
	n := time.Now().UnixNano() % (2*span + 1)
	return time.Duration(n) - d
}

// retry runs fn with the plan's backoff (1 s, 4 s, 16 s → three retries).
// Authentication and unsupported errors are never retried; Retry-After wins
// over the schedule. The overall budget is enforced by the caller's context.
func (c *Collector) retry(ctx context.Context, fn func() error) error {
	waits := []time.Duration{time.Second, 4 * time.Second, 16 * time.Second}
	var err error
	for attempt := 0; ; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if attempt >= len(waits) || !provider.Retryable(err) {
			return err
		}
		d := waits[attempt] + jitter(waits[attempt]/5)
		var he *provider.HTTPError
		if errors.As(err, &he) && he.RetryAfter > 0 {
			d = he.RetryAfter
		}
		if c.sleep != nil {
			if serr := c.sleep(ctx, d); serr != nil {
				return err
			}
		}
	}
}

// grain names the bucket size the store should assume.
func grain(d time.Duration) string {
	if d >= 24*time.Hour {
		return "day"
	}
	return "hour"
}

// rowHash makes re-imports idempotent: identical provider payloads collapse on
// the natural key, and a changed payload updates in place.
func rowHash(parts ...any) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%v\x1f", p)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}
