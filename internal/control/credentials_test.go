package control

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ai-observable/ai-observable/internal/vault"
)

func TestCredentialLifecycle(t *testing.T) {
	d := newDB(t)
	v := newVault(t)
	ctx := WithActor(context.Background(), "cli")
	secret := "sk-proj-lifecycle-0123456789abcdef"

	p, err := d.CreateProvider(ctx, Provider{Name: "openai", Kind: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.AddCredential(ctx, p.Name, "primary", "", secret, v)
	if err != nil {
		t.Fatalf("AddCredential: %v", err)
	}
	if c.Masked() != "sk-proj-…cdef" {
		t.Fatalf("masked = %q", c.Masked())
	}
	if c.Fingerprint != vault.Fingerprint(secret) || c.Status != "unverified" || c.Scope != "standard" {
		t.Fatalf("credential = %+v", c)
	}
	if _, err = d.AddCredential(ctx, p.ID, "dup", "", secret, v); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate key = %v, want ErrConflict", err)
	}

	got, err := d.Secret(ctx, c.ID, v)
	if err != nil || got != secret {
		t.Fatalf("Secret = %q, %v", got, err)
	}
	active, credID, err := d.ActiveSecretFor(ctx, p.Name, v)
	if err != nil || active != secret || credID != c.ID {
		t.Fatalf("ActiveSecretFor = %q, %q, %v", active, credID, err)
	}

	// Nothing on disk may contain the plaintext, including the WAL.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, rerr := os.ReadFile(d.Path() + suffix)
		if rerr != nil {
			continue
		}
		if bytes.Contains(b, []byte(secret)) {
			t.Fatalf("control.db%s contains the plaintext secret", suffix)
		}
	}

	if err = d.SetCredentialStatus(ctx, c.ID, "ok", ""); err != nil {
		t.Fatalf("SetCredentialStatus: %v", err)
	}
	okd, err := d.GetCredential(ctx, c.ID)
	if err != nil || okd.Status != "ok" || okd.LastOKAt == nil {
		t.Fatalf("status update = %+v, %v", okd, err)
	}
	if err = d.SetCredentialStatus(ctx, c.ID, "invalid", "401 invalid key"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = d.ActiveSecretFor(ctx, p.ID, v); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ActiveSecretFor with invalid status = %v, want ErrNotFound", err)
	}
	if err = d.SetCredentialStatus(ctx, c.ID, "ok", ""); err != nil {
		t.Fatal(err)
	}
	if err = d.TouchCredential(ctx, c.ID); err != nil {
		t.Fatalf("TouchCredential: %v", err)
	}
	touched, err := d.GetCredential(ctx, c.ID)
	if err != nil || touched.LastUsedAt == nil {
		t.Fatalf("last_used_at not stamped: %+v, %v", touched, err)
	}

	if err = d.RevokeCredential(ctx, c.ID); err != nil {
		t.Fatalf("RevokeCredential: %v", err)
	}
	if err = d.RevokeCredential(ctx, c.ID); err != nil {
		t.Fatalf("revoke twice must be idempotent: %v", err)
	}
	if _, _, err = d.ActiveSecretFor(ctx, p.ID, v); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ActiveSecretFor after revoke = %v, want ErrNotFound", err)
	}
	revoked, err := d.GetCredential(ctx, c.ID)
	if err != nil || !revoked.Revoked() {
		t.Fatalf("revoked row = %+v, %v", revoked, err)
	}
	list, err := d.ListCredentials(ctx, p.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListCredentials = %d, %v", len(list), err)
	}

	entries, err := d.AuditEntries(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Detail, secret) || strings.Contains(e.Detail, "cdef1") {
			t.Fatalf("audit detail leaked the secret: %s", e.Detail)
		}
	}
}

func TestActiveSecretPrefersOKCredential(t *testing.T) {
	d := newDB(t)
	v := newVault(t)
	ctx := context.Background()
	p, err := d.CreateProvider(ctx, Provider{Name: "anthropic", Kind: "anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := d.AddCredential(ctx, p.ID, "a", "", "sk-ant-api03-aaaaaaaaaaaaaaaaaaaa", v)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.AddCredential(ctx, p.ID, "b", "admin", "sk-ant-admin-bbbbbbbbbbbbbbbbbbbb", v)
	if err != nil {
		t.Fatal(err)
	}
	// The newest unverified key wins until one is proven good.
	if _, credID, err := d.ActiveSecretFor(ctx, p.ID, v); err != nil || credID != second.ID {
		t.Fatalf("newest unverified = %q, %v", credID, err)
	}
	if err = d.SetCredentialStatus(ctx, first.ID, "ok", ""); err != nil {
		t.Fatal(err)
	}
	if _, credID, err := d.ActiveSecretFor(ctx, p.ID, v); err != nil || credID != first.ID {
		t.Fatalf("preferred credential = %q, %v; want the ok one", credID, err)
	}
}

func TestLockedVaultNeverTouchesRows(t *testing.T) {
	d := newDB(t)
	locked, err := vault.Open(t.TempDir(), "disabled")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p, err := d.CreateProvider(ctx, Provider{Name: "openai", Kind: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.AddCredential(ctx, p.ID, "x", "", "sk-abcdef123456", locked); !errors.Is(err, ErrLocked) {
		t.Fatalf("AddCredential = %v, want ErrLocked", err)
	}
	if _, _, err = d.ActiveSecretFor(ctx, p.ID, locked); !errors.Is(err, ErrLocked) {
		t.Fatalf("ActiveSecretFor = %v, want ErrLocked", err)
	}
	list, err := d.ListCredentials(ctx, p.ID)
	if err != nil || len(list) != 0 {
		t.Fatalf("credentials = %d, %v", len(list), err)
	}
}

func TestCascadeDeletesCredentialsAndJobs(t *testing.T) {
	d := newDB(t)
	v := newVault(t)
	ctx := context.Background()
	p, err := d.CreateProvider(ctx, Provider{Name: "openai", Kind: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.AddCredential(ctx, p.ID, "k", "", "sk-abcdef12345678", v); err != nil {
		t.Fatal(err)
	}
	if err = d.DeleteProvider(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	var creds, jobs int
	if err = d.db.QueryRow("SELECT COUNT(*) FROM credentials").Scan(&creds); err != nil {
		t.Fatal(err)
	}
	if err = d.db.QueryRow("SELECT COUNT(*) FROM collector_jobs").Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if creds != 0 || jobs != 0 {
		t.Fatalf("after provider delete: credentials=%d jobs=%d, want 0/0", creds, jobs)
	}
}

func TestJobsFollowCapabilities(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	oa, err := d.CreateProvider(ctx, Provider{Name: "openai", Kind: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := d.ListJobs(ctx, oa.ID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]Job{}
	for _, j := range jobs {
		kinds[j.Kind] = j
	}
	for _, want := range []string{"models", "usage", "costs"} {
		if _, ok := kinds[want]; !ok {
			t.Fatalf("openai is missing the %s lane: %+v", want, jobs)
		}
	}
	if _, ok := kinds["balance"]; ok {
		t.Fatal("openai has no balance API; a balance lane must not be created")
	}
	ds, err := d.CreateProvider(ctx, Provider{Name: "deepseek", Kind: "deepseek"})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err = d.ListJobs(ctx, ds.ID)
	if err != nil {
		t.Fatal(err)
	}
	var hasBalance bool
	for _, j := range jobs {
		if j.Kind == "balance" {
			hasBalance = true
		}
		if j.Kind == "costs" {
			t.Fatal("deepseek exposes no cost history API")
		}
	}
	if !hasBalance {
		t.Fatal("deepseek must have a balance lane")
	}
}
