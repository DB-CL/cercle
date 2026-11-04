// Package wg fait tourner WireGuard dans le processus (wireguard-go et la pile
// TCP/IP userspace netstack) et réconcilie ses pairs à chaud.
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

// Key est une clé Curve25519 WireGuard (privée ou publique).
type Key [32]byte

// GenerateKey crée une clé privée aléatoire, bornée comme le fait WireGuard.
func GenerateKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return Key{}, fmt.Errorf("génération de clé : %w", err)
	}
	k[0] &= 248
	k[31] = (k[31] & 127) | 64
	return k, nil
}

// Public calcule la clé publique d'une clé privée.
func (k Key) Public() Key {
	pub, err := curve25519.X25519(k[:], curve25519.Basepoint)
	if err != nil {
		// X25519 n'échoue qu'avec une clé publique nulle en entrée, jamais avec la base.
		panic(err)
	}
	var p Key
	copy(p[:], pub)
	return p
}

// IsZero dit si la clé est vide.
func (k Key) IsZero() bool { return k == Key{} }

// String renvoie la clé en base64, la forme affichée et échangée entre nœuds.
func (k Key) String() string { return base64.StdEncoding.EncodeToString(k[:]) }

// Hex renvoie la clé en hexadécimal, la forme du protocole de configuration UAPI.
func (k Key) Hex() string { return hex.EncodeToString(k[:]) }

// MarshalText permet d'écrire la clé en JSON sous sa forme base64.
func (k Key) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// UnmarshalText lit une clé base64.
func (k *Key) UnmarshalText(b []byte) error {
	p, err := ParseKey(string(b))
	if err != nil {
		return err
	}
	*k = p
	return nil
}

// ParseKey lit une clé en base64.
func ParseKey(s string) (Key, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != 32 {
		return Key{}, errors.New("clé WireGuard invalide : 32 octets en base64 attendus")
	}
	var k Key
	copy(k[:], b)
	return k, nil
}

func parseHexKey(s string) (Key, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return Key{}, fmt.Errorf("clé hexadécimale invalide %q", s)
	}
	var k Key
	copy(k[:], b)
	return k, nil
}

// LoadOrCreateKey lit la clé privée du nœud, ou la crée au premier démarrage
// avec les droits 0600.
func LoadOrCreateKey(path string) (Key, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		k, perr := ParseKey(string(b))
		if perr != nil {
			return Key{}, fmt.Errorf("%s : %w", path, perr)
		}
		return k, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Key{}, fmt.Errorf("lecture de %s : %w", path, err)
	}
	k, err := GenerateKey()
	if err != nil {
		return Key{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Key{}, fmt.Errorf("création de %s : %w", filepath.Dir(path), err)
	}
	// Écriture dans un fichier temporaire puis renommage : jamais de clé à moitié écrite.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".node.key-*")
	if err != nil {
		return Key{}, fmt.Errorf("écriture de la clé : %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return Key{}, fmt.Errorf("droits de la clé : %w", err)
	}
	if _, err := tmp.WriteString(k.String() + "\n"); err != nil {
		tmp.Close()
		return Key{}, fmt.Errorf("écriture de la clé : %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return Key{}, fmt.Errorf("écriture de la clé : %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Key{}, fmt.Errorf("écriture de la clé : %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return Key{}, fmt.Errorf("écriture de la clé : %w", err)
	}
	return k, nil
}
