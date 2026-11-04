// Package index parcourt le dossier partagé, l'indexe dans SQLite et répond
// aux requêtes de navigation des pairs.
package index

import (
	"errors"
	"io/fs"
	"path"
	"strings"
)

// ErrBadPath signale un chemin refusé : absolu, avec « .. » ou malformé.
var ErrBadPath = errors.New("chemin refusé")

// ErrNotFound signale un chemin absent de l'index (inexistant ou exclu).
var ErrNotFound = errors.New("chemin introuvable")

// CleanPath valide un chemin relatif reçu d'un pair et le normalise : séparé
// par des /, sans / initial ni final, "" pour la racine. Tout chemin absolu ou
// contenant « .. » est refusé, même si son nettoyage retomberait dans le partage.
func CleanPath(p string) (string, error) {
	if p == "" || p == "." {
		return "", nil
	}
	if strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) {
		return "", ErrBadPath
	}
	for _, elem := range strings.Split(p, "/") {
		if elem == ".." {
			return "", ErrBadPath
		}
	}
	c := path.Clean(p)
	if c == "." {
		return "", nil
	}
	if !fs.ValidPath(c) {
		return "", ErrBadPath
	}
	return c, nil
}

// parentOf renvoie le dossier parent d'un chemin normalisé ("" pour la racine).
func parentOf(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}
