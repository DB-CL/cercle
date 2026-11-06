package index

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
)

// settingSelection stores what is shared at the first level of /share: "*"
// (everything, including what is mounted later) or a JSON list of names.
const settingSelection = "share.selection"

// Selection says which first-level entries of /share are shared.
type Selection struct {
	All     bool     `json:"all"`     // everything, including folders added later
	Folders []string `json:"folders"` // when All is false: the ticked names
}

// Shares reports whether a first-level entry is shared.
func (s Selection) Shares(name string) bool {
	return s.All || slices.Contains(s.Folders, name)
}

// Selection reads the selection; by default everything is shared.
func (ix *Index) Selection(ctx context.Context) (Selection, error) {
	v, ok, err := ix.st.Setting(ctx, settingSelection)
	if err != nil {
		return Selection{}, err
	}
	if !ok || v == "*" {
		return Selection{All: true}, nil
	}
	var names []string
	if err := json.Unmarshal([]byte(v), &names); err != nil {
		return Selection{}, fmt.Errorf("unreadable share selection: %w", err)
	}
	return Selection{Folders: names}, nil
}

// SetSelection stores the selection and triggers indexing: unticked entries
// leave the share.
func (ix *Index) SetSelection(ctx context.Context, sel Selection) error {
	value := "*"
	if !sel.All {
		names := []string{}
		for _, n := range sel.Folders {
			if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\x00") {
				return fmt.Errorf("invalid folder name: %q", n)
			}
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		b, _ := json.Marshal(names)
		value = string(b)
	}
	if err := ix.st.SetSetting(ctx, settingSelection, value); err != nil {
		return err
	}
	// What is no longer shared leaves the index right away, without waiting
	// for the scan: nothing of it is served from now on.
	if !sel.All {
		if err := ix.purgeUnshared(ctx, sel); err != nil {
			return err
		}
	}
	ix.TriggerScan()
	return nil
}

func (ix *Index) purgeUnshared(ctx context.Context, sel Selection) error {
	rows, err := ix.st.R.QueryContext(ctx, "SELECT path FROM files WHERE parent = '' AND path <> ''")
	if err != nil {
		return fmt.Errorf("reading the index: %w", err)
	}
	var gone []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil && !sel.Shares(p) {
			gone = append(gone, p)
		}
	}
	rows.Close()
	for _, p := range gone {
		if _, err := ix.st.W.ExecContext(ctx, "DELETE FROM files WHERE path = ? OR (path > ? AND path < ?)", p, p+"/", p+"0"); err != nil {
			return fmt.Errorf("removing %s from the index: %w", p, err)
		}
	}
	return nil
}

// TopEntry is a first-level entry of /share, for the settings page.
type TopEntry struct {
	Name   string `json:"name"`
	IsDir  bool   `json:"is_dir"`
	Shared bool   `json:"shared"`
	Size   int64  `json:"size"`  // from the index: 0 until shared and indexed
	Files  int64  `json:"files"` // same
}

// TopLevel lists the first level of /share (except always-excluded entries
// such as hidden files), folders first, with their sharing state.
func (ix *Index) TopLevel(ctx context.Context) ([]TopEntry, error) {
	sel, err := ix.Selection(ctx)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(ix.dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", ix.dir, err)
	}
	out := []TopEntry{}
	for _, d := range des {
		name := d.Name()
		if ix.excluded(name, name) {
			continue
		}
		isDir := d.IsDir()
		if d.Type()&os.ModeSymlink != 0 {
			if fi, err := os.Stat(ix.dir + "/" + name); err == nil {
				isDir = fi.IsDir()
			}
		}
		e := TopEntry{Name: name, IsDir: isDir, Shared: sel.Shares(name)}
		if e.Shared {
			if ent, err := ix.Lookup(ctx, name); err == nil {
				e.Size = ent.Size
				e.Files = ent.FileCount
				if !ent.IsDir {
					e.Files = 1
				}
			} else if !errors.Is(err, ErrNotFound) {
				return nil, err
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}
