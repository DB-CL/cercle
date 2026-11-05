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

// settingSelection enregistre ce qui est partagé au premier niveau de /share :
// « * » (tout, y compris ce qui sera monté plus tard) ou une liste JSON de noms.
const settingSelection = "share.selection"

// Selection dit quels éléments du premier niveau de /share sont partagés.
type Selection struct {
	All     bool     `json:"all"`     // tout, y compris les dossiers ajoutés plus tard
	Folders []string `json:"folders"` // si All est faux : les noms cochés
}

// Shares dit si un élément du premier niveau est partagé.
func (s Selection) Shares(name string) bool {
	return s.All || slices.Contains(s.Folders, name)
}

// Selection lit la sélection ; par défaut, tout est partagé.
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
		return Selection{}, fmt.Errorf("sélection du partage illisible : %w", err)
	}
	return Selection{Folders: names}, nil
}

// SetSelection enregistre la sélection et relance l'indexation : ce qui est
// décoché disparaît du partage à la fin du scan.
func (ix *Index) SetSelection(ctx context.Context, sel Selection) error {
	value := "*"
	if !sel.All {
		names := []string{}
		for _, n := range sel.Folders {
			if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\x00") {
				return fmt.Errorf("nom de dossier invalide : %q", n)
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
	// Ce qui n'est plus partagé disparaît de l'index tout de suite, sans
	// attendre la fin du scan : plus rien n'en est servi dès maintenant.
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
		return fmt.Errorf("lecture de l'index : %w", err)
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
			return fmt.Errorf("retrait de %s de l'index : %w", p, err)
		}
	}
	return nil
}

// TopEntry est un élément du premier niveau de /share, pour les réglages.
type TopEntry struct {
	Name   string `json:"name"`
	IsDir  bool   `json:"is_dir"`
	Shared bool   `json:"shared"`
	Size   int64  `json:"size"`  // d'après l'index : 0 tant qu'il n'est pas partagé et indexé
	Files  int64  `json:"files"` // idem
}

// TopLevel liste le premier niveau de /share (hors éléments toujours exclus,
// comme les fichiers cachés), dossiers d'abord, avec leur état de partage.
func (ix *Index) TopLevel(ctx context.Context) ([]TopEntry, error) {
	sel, err := ix.Selection(ctx)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(ix.dir)
	if err != nil {
		return nil, fmt.Errorf("lecture de %s : %w", ix.dir, err)
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
