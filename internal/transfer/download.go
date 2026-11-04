package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// download transfère un élément dans <dest>.part, en reprenant là où le .part
// s'est arrêté, puis le met en place : vérification de la taille, fsync,
// renommage atomique, date de modification distante.
func (m *Manager) download(ctx context.Context, it Item, j *job) error {
	peer, ok := m.opt.Peers(it.PeerID)
	if !ok {
		return errPermanent{errors.New("pair inconnu ou révoqué")}
	}
	root, err := os.OpenRoot(m.opt.Dir)
	if err != nil {
		return fmt.Errorf("dossier des téléchargements : %w", err)
	}
	defer root.Close()

	dest := it.DestPath
	part := dest + ".part"
	if fi, err := root.Stat(dest); err == nil && fi.Mode().IsRegular() && fi.Size() == it.Size {
		return nil // déjà là
	}
	if err := root.MkdirAll(path.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("création de %s : %w", path.Dir(dest), err)
	}

	var offset int64
	if fi, err := root.Stat(part); err == nil {
		offset = fi.Size()
	}
	if it.ETag == "" {
		offset = 0 // sans ETag, une reprise pourrait mélanger deux versions
	}
	resp, err := m.opt.Remote.Download(ctx, peer.IP, it.RemotePath, offset, it.ETag)
	var se statusError
	if offset > 0 && errors.As(err, &se) && se.HTTPStatus() == http.StatusRequestedRangeNotSatisfiable {
		// Le .part dépasse le fichier distant : on repart de zéro.
		offset = 0
		resp, err = m.opt.Remote.Download(ctx, peer.IP, it.RemotePath, 0, "")
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	total := resp.ContentLength
	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, size, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil || start != offset {
			return fmt.Errorf("reprise incohérente (%q pour l'octet %d)", resp.Header.Get("Content-Range"), offset)
		}
		total = size
	case http.StatusOK:
		offset = 0 // fichier modifié depuis (If-Range) ou première tentative
	default:
		return fmt.Errorf("réponse inattendue %d", resp.StatusCode)
	}
	if total < 0 {
		return errors.New("taille du fichier distant inconnue")
	}

	// Le fichier distant a pu changer depuis l'ajout : on garde sa nouvelle version.
	etag := resp.Header.Get("ETag")
	mtime := it.MTimeNano
	if v, err := strconv.ParseInt(resp.Header.Get(MtimeHeader), 10, 64); err == nil {
		mtime = v
	}
	if etag != it.ETag || total != it.Size || mtime != it.MTimeNano {
		if _, err := m.exec(context.WithoutCancel(ctx), `UPDATE queue SET etag = ?, size = ?, mtime = ? WHERE id = ?`, etag, total, mtime, it.ID); err != nil {
			return err
		}
	}

	f, err := root.OpenFile(part, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("ouverture de %s : %w", part, err)
	}
	defer f.Close()
	if err := f.Truncate(offset); err != nil {
		return fmt.Errorf("préparation de %s : %w", part, err)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("préparation de %s : %w", part, err)
	}
	j.startOffset.Store(offset)
	j.done.Store(offset)
	j.start.Store(time.Now().UnixNano())

	buf := make([]byte, burst)
	for written := offset; written < total; {
		n, rerr := throttledRead(ctx, m.limiter, resp.Body, buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return fmt.Errorf("écriture de %s : %w", part, err)
			}
			written += int64(n)
			j.done.Store(written)
		}
		if rerr == io.EOF {
			if written != total {
				return fmt.Errorf("transfert incomplet : %d octets sur %d", written, total)
			}
			break
		}
		if rerr != nil {
			return fmt.Errorf("transfert interrompu à l'octet %d : %w", written, rerr)
		}
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("écriture de %s : %w", part, err)
	}
	if fi, err := f.Stat(); err != nil || fi.Size() != total {
		return fmt.Errorf("taille finale incorrecte pour %s", part)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("écriture de %s : %w", part, err)
	}
	if err := root.Rename(part, dest); err != nil {
		return fmt.Errorf("mise en place de %s : %w", dest, err)
	}
	t := time.Unix(0, mtime)
	if err := root.Chtimes(dest, t, t); err != nil {
		m.log.Warn("date de modification non appliquée", "path", dest, "err", err)
	}
	return nil
}

// parseContentRange lit « bytes <début>-<fin>/<taille> ».
func parseContentRange(s string) (start, size int64, err error) {
	rest, ok := strings.CutPrefix(s, "bytes ")
	rng, sz, ok2 := strings.Cut(rest, "/")
	a, _, ok3 := strings.Cut(rng, "-")
	if !ok || !ok2 || !ok3 {
		return 0, 0, fmt.Errorf("Content-Range illisible : %q", s)
	}
	if start, err = strconv.ParseInt(a, 10, 64); err != nil {
		return 0, 0, err
	}
	if size, err = strconv.ParseInt(sz, 10, 64); err != nil {
		return 0, 0, err
	}
	return start, size, nil
}
