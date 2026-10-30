// Package provider defines the provider seam: kinds, capability reporting,
// preset metadata and the canonical usage normalizer.
//
// The package imports only the standard library. The eight adapter
// subpackages import this package; nothing here imports them.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ErrUnsupported is returned when a provider family exposes no API for the
// requested lane. It is never retried.
var ErrUnsupported = errors.New("provider: capability not supported")

// HTTPError is a non-2xx provider response with the provider's own message.
type HTTPError struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("provider: HTTP %d", e.Status)
	}
	return fmt.Sprintf("provider: HTTP %d: %s", e.Status, e.Message)
}

// StatusOf returns the HTTP status carried by err, or 0.
func StatusOf(err error) int {
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

// Retryable reports whether the collector should retry err. 401/403 and
// ErrUnsupported are never retried.
func Retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUnsupported) {
		return false
	}
	var he *HTTPError
	if errors.As(err, &he) {
		return he.Status == 429 || he.Status >= 500
	}
	// Transport errors (DNS, reset, timeout) are retryable.
	return true
}

// Credential is the in-memory secret view handed to an adapter. It is never
// persisted in this form.
type Credential struct {
	Secret string
	ID     string
	Label  string
	Scope  string
}

// Provider is the seam view of a configured provider.
type Provider struct {
	Kind        string
	BaseURL     string
	AuthStyle   string
	AuthMode    string
	StreamUsage bool
	Extra       map[string]string
}

// Target is one resolved provider plus its decrypted secret, as handed to the
// proxy. It carries no database handle so the proxy can cache it freely.
type Target struct {
	Name string
	ID   string
	Provider
	Secret     string
	ContextMax int64
}

// Window is a half-open provider usage window.
type Window struct {
	Start, End time.Time
}

// Usage is the canonical per-call usage.
type Usage struct {
	Input, Output, CacheRead, CacheWrite, Reasoning int64
	CostUSD                                         *float64
	Model, FinishReason                             string
}

// Empty reports whether nothing usable was parsed.
func (u Usage) Empty() bool {
	return u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 &&
		u.Reasoning == 0 && u.CostUSD == nil && u.Model == "" && u.FinishReason == ""
}

// UsageBucket is one provider-reported usage interval.
type UsageBucket struct {
	Model                   string
	Start, End              time.Time
	Requests, Input, Output int64
	CacheRead, CacheWrite   int64
	CostUSD                 *float64
	Project, APIKeyID       string
}

// CostBucket is one provider-reported money interval.
type CostBucket struct {
	Start, End         time.Time
	Amount             float64
	Currency, LineItem string
}

// Balance is prepaid credit.
type Balance struct {
	Amount   float64
	Currency string
}

// Identity is what a validation call learned about the credential.
type Identity struct {
	Account, Org, KeyLabel, KeyScope string
}

// ModelInfo is one catalog entry.
type ModelInfo struct {
	ID, DisplayName string
	ContextMax      int64
	Capabilities    map[string]bool
}

// Adapter is one provider family.
type Adapter interface {
	Kind() string
	Capabilities() Capabilities
	Validate(ctx context.Context, c Credential, p Provider) (Identity, error)
	ListModels(ctx context.Context, c Credential, p Provider) ([]ModelInfo, error)
	Usage(ctx context.Context, c Credential, p Provider, w Window) ([]UsageBucket, error)
	Costs(ctx context.Context, c Credential, p Provider, w Window) ([]CostBucket, error)
	Balance(ctx context.Context, c Credential, p Provider) (*Balance, error)
	ParseUsage(stream bool, contentType string, body []byte) (Usage, error)
}

// Client is the shared HTTP client for adapter calls.
func Client() *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			ResponseHeaderTimeout: 15 * time.Second,
			MaxIdleConns:          8,
			IdleConnTimeout:       60 * time.Second,
		},
	}
}
