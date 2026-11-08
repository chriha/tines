// Package secret encrypts credentials stored in the config file.
//
// Secrets are sealed with AES-256-GCM. The master key lives in the macOS
// Keychain when available, otherwise in a 0600 key file next to the config, so
// the config file alone (a backup, a git repo, a mirrored copy on a target) is
// not enough to recover a password.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/chriha/tines/internal/config"
)

// Prefix marks an encrypted value. It contains no quote characters, so it is
// safe inside an rclone connection string.
const Prefix = "enc:v1:"

const (
	kcService = "tines"
	kcAccount = "master-key"
)

var (
	mu  sync.Mutex
	key []byte
)

func IsEncrypted(s string) bool { return strings.HasPrefix(s, Prefix) }

func keyFile() string { return filepath.Join(filepath.Dir(config.Path()), "secret.key") }

func useKeychain() bool {
	if os.Getenv("TINES_NO_KEYCHAIN") != "" || runtime.GOOS != "darwin" {
		return false
	}
	_, err := exec.LookPath("security")
	return err == nil
}

// masterKey loads the key, creating it on first use.
func masterKey() ([]byte, error) {
	mu.Lock()
	defer mu.Unlock()
	if key != nil {
		return key, nil
	}
	if useKeychain() {
		out, err := exec.Command("security", "find-generic-password", "-s", kcService, "-a", kcAccount, "-w").Output()
		if err == nil {
			if k, err := hex.DecodeString(strings.TrimSpace(string(out))); err == nil && len(k) == 32 {
				key = k
				return key, nil
			}
		}
	}
	if b, err := os.ReadFile(keyFile()); err == nil {
		if k, err := hex.DecodeString(strings.TrimSpace(string(b))); err == nil && len(k) == 32 {
			key = k
			return key, nil
		}
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	h := hex.EncodeToString(k)
	if useKeychain() {
		if exec.Command("security", "add-generic-password", "-U", "-s", kcService, "-a", kcAccount, "-w", h).Run() == nil {
			key = k
			return key, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(keyFile()), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyFile(), []byte(h+"\n"), 0o600); err != nil {
		return nil, err
	}
	key = k
	return key, nil
}

func gcm() (cipher.AEAD, error) {
	k, err := masterKey()
	if err != nil {
		return nil, err
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// Encrypt seals plain text; the result starts with Prefix.
func Encrypt(plain string) (string, error) {
	g, err := gcm()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(g.Seal(nonce, nonce, []byte(plain), nil)), nil
}

// Decrypt opens a value produced by Encrypt.
func Decrypt(s string) (string, error) {
	if !IsEncrypted(s) {
		return "", errors.New("not an encrypted value")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, Prefix))
	if err != nil {
		return "", err
	}
	g, err := gcm()
	if err != nil {
		return "", err
	}
	if len(raw) < g.NonceSize() {
		return "", errors.New("encrypted value is truncated")
	}
	plain, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("cannot decrypt password (wrong or missing master key; re-add this target): %w", err)
	}
	return string(plain), nil
}
