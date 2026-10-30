// Package vault stores provider secrets encrypted at rest with AES-256-GCM.
//
// The master key is resolved once at start-up: the LLMTAP_MASTER_KEY
// environment variable, then <dir>/master.key, then a freshly generated key
// written with mode 0600. Nothing here is reversible without the key, and no
// plaintext secret is ever written to disk.
package vault

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrLocked is returned when the master key is unavailable: mode=disabled,
	// mode=env without the variable, or an unreadable key file.
	ErrLocked = errors.New("vault: master key unavailable")
	// ErrKeyMismatch is returned when the key on disk is not the key that
	// sealed the stored credentials. The key file is never overwritten.
	ErrKeyMismatch = errors.New("vault: master key does not match stored credentials")
)

const (
	keyFile = "master.key"
	kcvFile = "master.kcv"
	keyEnv  = "LLMTAP_MASTER_KEY"
)

// Vault seals and opens credentials with one 32-byte key.
type Vault struct {
	key    [32]byte
	kcv    string
	path   string
	usable bool
}

// Open resolves the master key.
//
//	mode=file (default)  env → <dir>/master.key → generate
//	mode=env             env only; missing returns ErrLocked
//	mode=disabled        no key; Seal/Open return ErrLocked
func Open(dir, mode string) (*Vault, error) {
	switch mode {
	case "", "file":
		return openFile(dir)
	case "env":
		raw := os.Getenv(keyEnv)
		if strings.TrimSpace(raw) == "" {
			return &Vault{}, ErrLocked
		}
		key, err := parseKey(raw)
		if err != nil {
			return nil, err
		}
		return &Vault{key: key, kcv: kcvOf(key), usable: true}, nil
	case "disabled":
		return &Vault{}, nil
	default:
		return nil, fmt.Errorf("vault: unknown mode %q (want file, env, or disabled)", mode)
	}
}

func openFile(dir string) (*Vault, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("vault: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, keyFile)
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("vault: read master key: %w", err)
	}
	if os.IsNotExist(err) {
		key, gerr := newKey()
		if gerr != nil {
			return nil, gerr
		}
		installedByUs, werr := writeKeyFile(path, key)
		if werr != nil {
			return nil, werr
		}
		if !installedByUs {
			installed, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil, fmt.Errorf("vault: read installed master key: %w", rerr)
			}
			if key, err = parseKey(string(installed)); err != nil {
				return nil, fmt.Errorf("vault: installed master key: %w", err)
			}
		}
		kcv := kcvOf(key)
		// The sidecar is advisory: failing to write it must not lose the key.
		_ = os.WriteFile(filepath.Join(dir, kcvFile), []byte(kcv), 0600)
		return &Vault{key: key, kcv: kcv, path: path, usable: true}, nil
	}
	key, err := parseKey(string(b))
	if err != nil {
		return nil, fmt.Errorf("vault: %s: %w", path, err)
	}
	v := &Vault{key: key, kcv: kcvOf(key), path: path, usable: true}
	if stored, rerr := os.ReadFile(filepath.Join(dir, kcvFile)); rerr == nil {
		if s := strings.TrimSpace(string(stored)); s != "" && !strings.EqualFold(s, v.kcv) {
			return nil, ErrKeyMismatch
		}
	}
	return v, nil
}

// Available reports whether the vault can seal and open.
func (v *Vault) Available() bool { return v != nil && v.usable }

// KCV is hex(sha256(key))[:16]: a key check value that is safe to store next to
// the ciphertext and safe to show in diagnostics.
func (v *Vault) KCV() string {
	if v == nil {
		return ""
	}
	return v.kcv
}

// Path returns the key file path, or "" when the key came from the environment.
func (v *Vault) Path() string {
	if v == nil {
		return ""
	}
	return v.path
}

// VerifyKCV compares this key against a stored check value. An empty stored
// value means "not recorded yet" and always passes.
func (v *Vault) VerifyKCV(stored string) error {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return nil
	}
	if v == nil || !v.usable {
		return ErrLocked
	}
	if !strings.EqualFold(stored, v.kcv) {
		return ErrKeyMismatch
	}
	return nil
}

// Rotate generates a new master key in dir. The previous key file is kept as
// master.key.old so the caller can re-seal existing credentials with it, then
// delete it. Only mode=file vaults can rotate.
func (v *Vault) Rotate(dir string) error {
	if v == nil || !v.usable {
		return ErrLocked
	}
	if v.path == "" {
		return errors.New("vault: rotation requires mode=file")
	}
	old, err := os.ReadFile(v.path)
	if err != nil {
		return fmt.Errorf("vault: read current key: %w", err)
	}
	key, err := newKey()
	if err != nil {
		return err
	}
	if err = os.WriteFile(v.path+".old", old, 0600); err != nil {
		return fmt.Errorf("vault: keep previous key: %w", err)
	}
	if _, err = writeKeyFile(v.path, key); err != nil {
		return err
	}
	v.key, v.kcv, v.path = key, kcvOf(key), filepath.Join(dir, keyFile)
	return os.WriteFile(filepath.Join(dir, kcvFile), []byte(v.kcv), 0600)
}
