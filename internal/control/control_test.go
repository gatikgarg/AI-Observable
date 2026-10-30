package control

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ai-observable/ai-observable/internal/config"
	"github.com/ai-observable/ai-observable/internal/vault"
)

func newDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func newVault(t *testing.T) *vault.Vault {
	t.Helper()
	v, err := vault.Open(filepath.Join(t.TempDir(), "keys"), "file")
	if err != nil {
		t.Fatalf("vault.Open: %v", err)
	}
	return v
}

func TestOpenAppliesPragmasOnEveryConnection(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		conn, err := d.db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn: %v", err)
		}
		var fk, timeout int
		if err = conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			conn.Close()
			t.Fatalf("PRAGMA foreign_keys: %v", err)
		}
		if err = conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
			conn.Close()
			t.Fatalf("PRAGMA busy_timeout: %v", err)
		}
		var mode string
		if err = conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			conn.Close()
			t.Fatalf("PRAGMA journal_mode: %v", err)
		}
		conn.Close()
		if fk != 1 || timeout != 5000 || !strings.EqualFold(mode, "wal") {
			t.Fatalf("connection %d: foreign_keys=%d busy_timeout=%d journal_mode=%q", i, fk, timeout, mode)
		}
	}
	// Reopening an existing database must be idempotent.
	path := d.Path()
	d.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	if v, err := again.Meta(ctx, "schema_version"); err != nil || v != "2" {
		t.Fatalf("schema_version=%q, %v", v, err)
	}
}

func TestProviderCRUD(t *testing.T) {
	d := newDB(t)
	ctx := WithActor(context.Background(), "ui")

	p, err := d.CreateProvider(ctx, Provider{Name: "openai-main", Kind: "openai"})
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if p.ID == "" || p.BaseURL != "https://api.openai.com" {
		t.Fatalf("created provider = %+v", p)
	}
	if p.AuthStyle != "bearer" || p.AuthMode != "managed" || !p.Enabled || !p.StreamUsage {
		t.Fatalf("defaults not applied: %+v", p)
	}
	if p.Origin != "ui" {
		t.Fatalf("origin = %q, want ui", p.Origin)
	}

	if _, err = d.CreateProvider(ctx, Provider{Name: "openai-main", Kind: "openai"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate create = %v, want ErrConflict", err)
	}
	byName, err := d.GetProvider(ctx, "openai-main")
	if err != nil || byName.ID != p.ID {
		t.Fatalf("GetProvider(name) = %+v, %v", byName, err)
	}

	p.BaseURL = "https://gateway.internal/v1"
	p.ReconcileMode = "gap_fill"
	p.StreamUsage = false
	updated, err := d.UpdateProvider(ctx, p)
	if err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}
	if updated.BaseURL != "https://gateway.internal/v1" || updated.ReconcileMode != "gap_fill" || updated.StreamUsage {
		t.Fatalf("update not applied: %+v", updated)
	}
	if updated.CreatedAt != p.CreatedAt {
		t.Fatal("update changed created_at_ms")
	}

	entries, err := d.AuditEntries(ctx, 10)
	if err != nil {
		t.Fatalf("AuditEntries: %v", err)
	}
	actions := map[string]int{}
	for _, e := range entries {
		actions[e.Action]++
		if e.Actor != "ui" {
			t.Fatalf("audit actor = %q, want ui", e.Actor)
		}
	}
	if actions["provider.create"] != 1 || actions["provider.update"] != 1 {
		t.Fatalf("audit actions = %v", actions)
	}

	if err = d.DeleteProvider(ctx, p.ID); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}
	if _, err = d.GetProvider(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
}

func TestProviderValidation(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	cases := []Provider{
		{Name: "Bad Name", Kind: "openai"},
		{Name: "a", Kind: "openai"},
		{Name: "okname", Kind: "nope"},
		{Name: "okname", Kind: "openai-compatible"},
		{Name: "okname", Kind: "openai", BaseURL: "ftp://example.com"},
		{Name: "okname", Kind: "openai", BaseURL: "https://user:pw@example.com"},
		{Name: "okname", Kind: "openai", AuthStyle: "magic"},
		{Name: "okname", Kind: "openai", AuthMode: "sometimes"},
		{Name: "okname", Kind: "openai", ReconcileMode: "mirror-all"},
	}
	for i, in := range cases {
		if _, err := d.CreateProvider(ctx, in); !errors.Is(err, ErrInvalid) {
			t.Fatalf("case %d (%+v) err = %v, want ErrInvalid", i, in, err)
		}
	}
}

func TestConfigImportIsReadOnlyAndKeepsUIRows(t *testing.T) {
	d := newDB(t)
	ctx := context.Background()
	cfg := map[string]config.Provider{
		"openai": {Upstream: "https://legacy.example.com", StreamUsage: true},
		"my-gw":  {Upstream: "https://gw.example.com/v1"},
	}
	n, err := d.ImportFromConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("ImportFromConfig: %v", err)
	}
	if n != 2 {
		t.Fatalf("imported %d, want 2", n)
	}
	gh, err := d.GetProvider(ctx, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if gh.Origin != "config" || gh.AuthMode != "client" || gh.BaseURL != "https://legacy.example.com" {
		t.Fatalf("imported provider = %+v", gh)
	}
	other, err := d.GetProvider(ctx, "my-gw")
	if err != nil {
		t.Fatal(err)
	}
	if other.Kind != "openai-compatible" {
		t.Fatalf("unknown config provider kind = %q", other.Kind)
	}
	if _, err = d.UpdateProvider(ctx, gh); !errors.Is(err, ErrInvalid) {
		t.Fatalf("update imported = %v, want ErrInvalid", err)
	}
	if err = d.DeleteProvider(ctx, gh.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("delete imported = %v, want ErrInvalid", err)
	}
	if n, err = d.ImportFromConfig(ctx, cfg); err != nil || n != 0 {
		t.Fatalf("second import = %d, %v; want 0", n, err)
	}
	// A UI provider with the same name is never overwritten by an import.
	if _, err = d.CreateProvider(ctx, Provider{Name: "ui-owned", Kind: "groq"}); err != nil {
		t.Fatal(err)
	}
	if n, err = d.ImportFromConfig(ctx, map[string]config.Provider{
		"ui-owned": {Upstream: "https://evil.example.com"},
	}); err != nil || n != 0 {
		t.Fatalf("import over UI row = %d, %v; want 0", n, err)
	}
	kept, err := d.GetProvider(ctx, "ui-owned")
	if err != nil || kept.BaseURL != "https://api.groq.com/openai" {
		t.Fatalf("UI provider overwritten: %+v, %v", kept, err)
	}
}
