// Package wg runs WireGuard in-process (wireguard-go and the user-space
// netstack TCP/IP stack) and reconciles its peers live.
package wg

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// Key is a WireGuard Curve25519 key (private or public).
type Key [32]byte

// GenerateKey creates a random private key, clamped as WireGuard does.
func GenerateKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return Key{}, fmt.Errorf("key generation: %w", err)
	}
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	return k, nil
}

// Public computes the public key of a private key.
func (k Key) Public() Key {
	pub, err := curve25519.X25519(k[:], curve25519.Basepoint)
	if err != nil {
		// X25519 only fails on an all-zero public input, never with the base point.
		panic(err)
	}
	var p Key
	copy(p[:], pub)
	return p
}

// IsZero reports whether the key is empty.
func (k Key) IsZero() bool { return k == Key{} }

// String returns the key in base64, the form shown and exchanged between nodes.
func (k Key) String() string { return base64.StdEncoding.EncodeToString(k[:]) }

// Hex returns the key in hexadecimal, the form used by the UAPI configuration protocol.
func (k Key) Hex() string { return hex.EncodeToString(k[:]) }

// MarshalText writes the key to JSON in its base64 form.
func (k Key) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// UnmarshalText reads a base64 key.
func (k *Key) UnmarshalText(b []byte) error {
	p, err := ParseKey(string(b))
	if err != nil {
		return err
	}
	*k = p
	return nil
}

// ParseKey reads a base64 key.
func ParseKey(s string) (Key, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 32 {
		return Key{}, errors.New("invalid WireGuard key: expected 32 bytes in base64")
	}
	var k Key
	copy(k[:], b)
	return k, nil
}

func parseHexKey(s string) (Key, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return Key{}, fmt.Errorf("invalid hexadecimal key %q", s)
	}
	var k Key
	copy(k[:], b)
	return k, nil
}

// LoadOrCreateKey reads the node's private key, or creates it on first start
// with 0600 permissions.
func LoadOrCreateKey(path string) (Key, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		k, perr := ParseKey(string(b))
		if perr != nil {
			return Key{}, fmt.Errorf("%s: %w", path, perr)
		}
		return k, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Key{}, fmt.Errorf("reading %s: %w", path, err)
	}
	k, err := GenerateKey()
	if err != nil {
		return Key{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Key{}, fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	// Write to a temporary file then rename: never a half-written key.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".node.key-*")
	if err != nil {
		return Key{}, fmt.Errorf("writing the key: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return Key{}, fmt.Errorf("key permissions: %w", err)
	}
	if _, err := tmp.WriteString(k.String() + "\n"); err != nil {
		tmp.Close()
		return Key{}, fmt.Errorf("writing the key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return Key{}, fmt.Errorf("writing the key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Key{}, fmt.Errorf("writing the key: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return Key{}, fmt.Errorf("writing the key: %w", err)
	}
	return k, nil
}
