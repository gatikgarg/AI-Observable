package vault

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// newKey returns 32 bytes from the OS CSPRNG.
func newKey() ([32]byte, error) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return key, fmt.Errorf("vault: generate master key: %w", err)
	}
	return key, nil
}

// parseKey accepts 64 hex characters, base64 (padded or raw), or 32 raw bytes.
func parseKey(s string) ([32]byte, error) {
	var out [32]byte
	s = strings.TrimSpace(s)
	if s == "" {
		return out, fmt.Errorf("vault: empty master key")
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		copy(out[:], b)
		return out, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == 32 {
			copy(out[:], b)
			return out, nil
		}
	}
	if len(s) == 32 {
		copy(out[:], s)
		return out, nil
	}
	return out, fmt.Errorf("vault: master key must be 32 bytes (64 hex characters, base64, or 32 raw bytes); got %d characters", len(s))
}

// kcvOf is the key check value: hex(sha256(key))[:16].
func kcvOf(key [32]byte) string {
	sum := sha256.Sum256(key[:])
	return hex.EncodeToString(sum[:])[:16]
}

// writeKeyFile writes the key atomically with mode 0600. On Windows the mode is
// advisory, so a failed chmod is ignored rather than fatal.
func writeKeyFile(path string, key [32]byte) (installedByUs bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, fmt.Errorf("vault: create key directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "master-*.tmp")
	if err != nil {
		return false, fmt.Errorf("vault: write master key: %w", err)
	}
	name := tmp.Name()
	_, statErr := os.Stat(path)
	existed := statErr == nil
	defer os.Remove(name)
	if _, err = tmp.WriteString(hex.EncodeToString(key[:]) + "\n"); err != nil {
		tmp.Close()
		return false, fmt.Errorf("vault: write master key: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return false, fmt.Errorf("vault: write master key: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return false, fmt.Errorf("vault: write master key: %w", err)
	}
	_ = os.Chmod(name, 0600)
	for attempt := 0; attempt < 5; attempt++ {
		if err = os.Rename(name, path); err == nil {
			return true, nil
		}
		// Windows security/indexing software can hold a just-created file for a
		// few milliseconds. Retry without changing the atomic-install contract.
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	if err != nil {
		// Some Windows endpoint-security products deny rename on a newly
		// created key even though exclusive creation is allowed. The initial
		// key has no prior value to replace, so an O_EXCL create remains safe.
		if !existed {
			_ = os.Remove(name)
			out, createErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if os.IsExist(createErr) {
				return false, nil
			}
			if createErr != nil {
				return false, fmt.Errorf("vault: install master key: %w", err)
			}
			_, writeErr := out.WriteString(hex.EncodeToString(key[:]) + "\n")
			if writeErr == nil {
				writeErr = out.Sync()
			}
			closeErr := out.Close()
			if writeErr == nil {
				writeErr = closeErr
			}
			if writeErr != nil {
				_ = os.Remove(path)
				return false, fmt.Errorf("vault: install master key: %w", writeErr)
			}
			_ = os.Chmod(path, 0600)
			return true, nil
		}
		return false, fmt.Errorf("vault: install master key: %w", err)
	}
	_ = os.Chmod(path, 0600)
	return false, fmt.Errorf("vault: install master key: rename retries exhausted")
}
