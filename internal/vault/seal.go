package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// aad binds a ciphertext to the credential and provider it belongs to, so a
// row copied from one credential to another fails to decrypt.
func aad(credentialID, providerID string) []byte {
	return []byte(credentialID + "\x00" + providerID)
}

func (v *Vault) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(v.key[:])
	if err != nil {
		return nil, fmt.Errorf("vault: cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// Seal encrypts secret with a fresh 12-byte nonce every call. The nonce is
// returned separately so it can be stored in its own column.
func (v *Vault) Seal(credentialID, providerID string, secret []byte) (ciphertext, nonce []byte, err error) {
	if v == nil || !v.usable {
		return nil, nil, ErrLocked
	}
	g, err := v.gcm()
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, g.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("vault: nonce: %w", err)
	}
	ct := g.Seal(nil, nonce, secret, aad(credentialID, providerID))
	return ct, nonce, nil
}

// Open decrypts a sealed secret. A wrong key or a moved row returns an error
// and never returns partial plaintext.
func (v *Vault) Open(credentialID, providerID string, ciphertext, nonce []byte) ([]byte, error) {
	if v == nil || !v.usable {
		return nil, ErrLocked
	}
	g, err := v.gcm()
	if err != nil {
		return nil, err
	}
	if len(nonce) != g.NonceSize() {
		return nil, fmt.Errorf("vault: nonce must be %d bytes, got %d", g.NonceSize(), len(nonce))
	}
	if len(ciphertext) < g.Overhead() {
		return nil, errors.New("vault: ciphertext is truncated")
	}
	plain, err := g.Open(nil, nonce, ciphertext, aad(credentialID, providerID))
	if err != nil {
		return nil, fmt.Errorf("vault: decrypt credential %s: %w", credentialID, err)
	}
	return plain, nil
}

// Fingerprint is hex(sha256(secret))[:8]: stable, non-reversible, used for
// dedupe, session keys and key_fingerprint in calls.
func Fingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])[:8]
}

// schemePrefixes are the recognizable key prefixes reported as key_prefix.
// Longer prefixes are listed first so "sk-ant-…" never degrades to "sk-".
var schemePrefixes = []string{
	"sk-ant-admin-", "sk-ant-oat-", "sk-ant-", "sk-admin-", "sk-proj-", "sk-or-", "sk-svcacct-",
	"sk-", "gsk_", "or-", "AIza", "xai-", "api-",
}

// Mask returns the display parts of a secret: its scheme prefix and its last
// four characters. The middle of the key is never returned, so no mask is
// reversible. A secret shorter than 12 characters reports no last4: showing
// four of ten characters would give away too much.
func Mask(secret string) (prefix, last4 string) {
	secret = strings.TrimSpace(secret)
	for _, p := range schemePrefixes {
		if strings.HasPrefix(secret, p) {
			prefix = p
			break
		}
	}
	if len(secret) >= 12 {
		last4 = secret[len(secret)-4:]
	}
	return prefix, last4
}

// Display renders the canonical masked form, e.g. "sk-…4f2a".
func Display(prefix, last4 string) string {
	if last4 == "" {
		return prefix + "…"
	}
	return prefix + "…" + last4
}
