// Package index walks the shared folder, indexes it in SQLite and answers
// the peers' browsing requests.
package index

import (
	"errors"
	"io/fs"
	"path"
	"strings"
)

// ErrBadPath reports a refused path: absolute, containing "..", or malformed.
var ErrBadPath = errors.New("path refused")

// ErrNotFound reports a path missing from the index (absent or excluded).
var ErrNotFound = errors.New("path not found")

// CleanPath validates a relative path received from a peer and normalizes it:
// slash-separated, no leading or trailing slash, "" for the root. Any absolute
// path or path containing ".." is refused, even if cleaning it would land back
// inside the share.
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

// parentOf returns the parent folder of a normalized path ("" for the root).
func parentOf(p string) string {
	d := path.Dir(p)
	if d == "." {
		return ""
	}
	return d
}
