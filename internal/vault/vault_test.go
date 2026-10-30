package vault

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	v, err := Open(t.TempDir(), "file")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !v.Available() {
		t.Fatal("file mode vault must be available")
	}
	secret := []byte("sk-proj-abcdefghijklmnopqrstuvwxyz0123456789")
	ct1, n1, err := v.Seal("cred-1", "prov-1", secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(ct1, secret) {
		t.Fatal("ciphertext contains the plaintext secret")
	}
	if len(n1) != 12 {
		t.Fatalf("nonce length = %d, want 12", len(n1))
	}
	ct2, n2, err := v.Seal("cred-1", "prov-1", secret)
	if err != nil {
		t.Fatalf("Seal 2: %v", err)
	}
	if bytes.Equal(n1, n2) || bytes.Equal(ct1, ct2) {
		t.Fatal("Seal reused a nonce: two seals of one secret must differ")
	}

	got, err := v.Open("cred-1", "prov-1", ct1, n1)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("Open returned %q, want the sealed secret", got)
	}

	if _, err = v.Open("cred-2", "prov-1", ct1, n1); err == nil {
		t.Fatal("Open accepted a ciphertext moved to another credential")
	}
	if _, err = v.Open("cred-1", "prov-2", ct1, n1); err == nil {
		t.Fatal("Open accepted a ciphertext moved to another provider")
	}
	if _, err = v.Open("cred-1", "prov-1", ct1[:8], n1); err == nil {
		t.Fatal("Open accepted a truncated ciphertext")
	}
	if _, err = v.Open("cred-1", "prov-1", ct1, n1[:8]); err == nil {
		t.Fatal("Open accepted a short nonce")
	}
}

func TestDisabledVaultIsLocked(t *testing.T) {
	v, err := Open(t.TempDir(), "disabled")
	if err != nil {
		t.Fatalf("Open disabled: %v", err)
	}
	if v.Available() {
		t.Fatal("disabled vault reports Available")
	}
	if _, _, err = v.Seal("c", "p", []byte("x")); !errors.Is(err, ErrLocked) {
		t.Fatalf("Seal err = %v, want ErrLocked", err)
	}
	if _, err = v.Open("c", "p", []byte("0123456789abcdef"), make([]byte, 12)); !errors.Is(err, ErrLocked) {
		t.Fatalf("Open err = %v, want ErrLocked", err)
	}
	if _, err = Open(t.TempDir(), "nonsense"); err == nil {
		t.Fatal("Open accepted an unknown mode")
	}
}

func TestEnvMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(keyEnv, "")
	if _, err := Open(dir, "env"); !errors.Is(err, ErrLocked) {
		t.Fatalf("Open without env = %v, want ErrLocked", err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	t.Setenv(keyEnv, hex.EncodeToString(key))
	v, err := Open(dir, "env")
	if err != nil {
		t.Fatalf("Open with env: %v", err)
	}
	ct, nonce, err := v.Seal("c", "p", []byte("secret-value"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	again, err := Open(dir, "env")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	plain, err := again.Open("c", "p", ct, nonce)
	if err != nil || string(plain) != "secret-value" {
		t.Fatalf("Open = %q, %v", plain, err)
	}
	if v.Path() != "" {
		t.Fatal("env mode must not report a key file path")
	}
	if _, err = os.Stat(filepath.Join(dir, keyFile)); !os.IsNotExist(err) {
		t.Fatal("env mode must not create a key file")
	}

	t.Setenv(keyEnv, "not-a-key")
	if _, err = Open(dir, "env"); err == nil {
		t.Fatal("Open accepted a malformed env key")
	}
}

func TestKeyFileAndKCV(t *testing.T) {
	dir := t.TempDir()
	v, err := Open(dir, "file")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	kp := filepath.Join(dir, keyFile)
	st, err := os.Stat(kp)
	if err != nil {
		t.Fatalf("stat master key: %v", err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0600 {
		t.Fatalf("master.key mode = %o, want 600", st.Mode().Perm())
	}
	b, err := os.ReadFile(kp)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(b))) != 64 {
		t.Fatalf("master.key holds %d characters, want 64 hex", len(strings.TrimSpace(string(b))))
	}
	if len(v.KCV()) != 16 {
		t.Fatalf("KCV = %q, want 16 hex characters", v.KCV())
	}
	if err = v.VerifyKCV(v.KCV()); err != nil {
		t.Fatalf("VerifyKCV(own) = %v", err)
	}
	if err = v.VerifyKCV("0000000000000000"); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("VerifyKCV(other) = %v, want ErrKeyMismatch", err)
	}

	reopened, err := Open(dir, "file")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.KCV() != v.KCV() {
		t.Fatalf("reopened KCV = %q, want %q", reopened.KCV(), v.KCV())
	}

	// A different key on disk with the original KCV sidecar must fail loudly and
	// must not be replaced by a freshly generated key.
	other := make([]byte, 32)
	if _, err = rand.Read(other); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(kp, []byte(hex.EncodeToString(other)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Open(dir, "file"); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("Open with wrong key = %v, want ErrKeyMismatch", err)
	}
	after, err := os.ReadFile(kp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), hex.EncodeToString(other)) {
		t.Fatal("Open overwrote an existing master key")
	}
}

func TestRotateReSealsWithNewKey(t *testing.T) {
	dir := t.TempDir()
	v1, err := Open(dir, "file")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	oldCT, oldNonce, err := v1.Seal("c1", "p1", []byte("first-secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	kcvBefore := v1.KCV()
	if err = v1.Rotate(dir); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if v1.KCV() == kcvBefore {
		t.Fatal("Rotate kept the same key")
	}
	if _, err = v1.Open("c1", "p1", oldCT, oldNonce); err == nil {
		t.Fatal("the rotated vault opened ciphertext sealed with the old key")
	}
	oldKey, err := os.ReadFile(filepath.Join(dir, keyFile+".old"))
	if err != nil {
		t.Fatalf("read previous key: %v", err)
	}
	prevKey, err := parseKey(string(oldKey))
	if err != nil {
		t.Fatal(err)
	}
	prev := &Vault{key: prevKey, kcv: kcvOf(prevKey), usable: true}
	plain, err := prev.Open("c1", "p1", oldCT, oldNonce)
	if err != nil || string(plain) != "first-secret" {
		t.Fatalf("previous key Open = %q, %v", plain, err)
	}
	reopened, err := Open(dir, "file")
	if err != nil {
		t.Fatalf("reopen after rotate: %v", err)
	}
	if reopened.KCV() != v1.KCV() {
		t.Fatal("rotated key was not persisted")
	}
}

func TestFingerprintAndMask(t *testing.T) {
	secret := "sk-ant-api03-abcdefghijklmnopqrstuvwxyz"
	prefix, last4 := Mask(secret)
	if prefix != "sk-ant-" {
		t.Fatalf("prefix = %q, want sk-ant-", prefix)
	}
	if last4 != "wxyz" {
		t.Fatalf("last4 = %q, want wxyz", last4)
	}
	if Display(prefix, last4) != "sk-ant-…wxyz" {
		t.Fatalf("Display = %q", Display(prefix, last4))
	}
	if p, l := Mask("gsk_1234567890abcdef"); p != "gsk_" || l != "cdef" {
		t.Fatalf("Mask(groq) = %q, %q", p, l)
	}
	if p, l := Mask("short"); p != "" || l != "" {
		t.Fatalf("Mask(short) = %q, %q; a short secret must not reveal 4 characters", p, l)
	}
	if Fingerprint(secret) != Fingerprint(secret) {
		t.Fatal("Fingerprint is not stable")
	}
	if len(Fingerprint(secret)) != 8 {
		t.Fatalf("Fingerprint length = %d, want 8", len(Fingerprint(secret)))
	}
	if Fingerprint(secret) == Fingerprint(secret+"x") {
		t.Fatal("Fingerprint collided on a one-character change")
	}
}

func TestSecretNeverWrittenToDisk(t *testing.T) {
	dir := t.TempDir()
	v, err := Open(dir, "file")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	secret := "sk-live-very-secret-value-0123456789"
	ct, nonce, err := v.Seal("c9", "p9", []byte(secret))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	blob := append(append([]byte{}, ct...), nonce...)
	if err = os.WriteFile(filepath.Join(dir, "row.bin"), blob, 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		b, rerr := os.ReadFile(full)
		if rerr != nil {
			t.Fatal(rerr)
		}
		if bytes.Contains(b, []byte(secret)) {
			t.Fatalf("%s contains the plaintext secret", e.Name())
		}
	}
}
