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

// download transfers an item into its partial file, resuming where it
// stopped, then puts it in place: size check, fsync, atomic rename, remote
// modification time.
func (m *Manager) download(ctx context.Context, it Item, j *job) error {
	peer, ok := m.opt.Peers(it.PeerID)
	if !ok {
		return errPermanent{errors.New("unknown or revoked peer")}
	}
	root, err := os.OpenRoot(m.opt.Dir)
	if err != nil {
		return fmt.Errorf("downloads folder: %w", err)
	}
	defer root.Close()

	dest := it.DestPath
	part := partPath(it)
	if fi, err := root.Stat(dest); err == nil && fi.Mode().IsRegular() && fi.Size() == it.Size {
		return nil // already there
	}
	if err := root.MkdirAll(path.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", path.Dir(dest), err)
	}

	var offset int64
	if fi, err := root.Stat(part); err == nil {
		offset = fi.Size()
	}
	if it.ETag == "" {
		offset = 0 // without an ETag, resuming could mix two versions
	}
	resp, err := m.opt.Remote.Download(ctx, peer.IP, it.RemotePath, offset, it.ETag)
	var se statusError
	if offset > 0 && errors.As(err, &se) && se.HTTPStatus() == http.StatusRequestedRangeNotSatisfiable {
		// The partial file is larger than the remote file: restart from zero.
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
			return fmt.Errorf("inconsistent resume (%q for byte %d)", resp.Header.Get("Content-Range"), offset)
		}
		total = size
	case http.StatusOK:
		offset = 0 // file changed since (If-Range) or first attempt
	default:
		return fmt.Errorf("unexpected response %d", resp.StatusCode)
	}
	if total < 0 {
		return errors.New("unknown remote file size")
	}

	// The remote file may have changed since it was queued: keep its new version.
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
		return fmt.Errorf("opening %s: %w", part, err)
	}
	defer f.Close()
	// Disk space: a peer must not be able to fill the disk, even by announcing a
	// small file and then sending much more.
	if free, ok := diskFree(m.opt.Dir); ok && free-(total-offset) < minFree {
		return fmt.Errorf("not enough disk space: %d bytes to receive, %d free (reserve %d)", total-offset, free, int64(minFree))
	}
	if err := f.Truncate(offset); err != nil {
		return fmt.Errorf("preparing %s: %w", part, err)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("preparing %s: %w", part, err)
	}
	j.startOffset.Store(offset)
	j.done.Store(offset)
	j.start.Store(time.Now().UnixNano())

	buf := make([]byte, burst)
	sinceCheck := int64(0)
	for written := offset; written < total; {
		chunk := buf
		if rem := total - written; rem < int64(len(chunk)) {
			chunk = chunk[:rem] // never more than the announced size
		}
		n, rerr := throttledRead(ctx, m.limiter, resp.Body, chunk)
		sinceCheck += int64(n)
		if sinceCheck >= diskCheckEvery {
			sinceCheck = 0
			if free, ok := diskFree(m.opt.Dir); ok && free < minFree {
				return fmt.Errorf("not enough disk space: transfer stopped at byte %d", written)
			}
		}
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return fmt.Errorf("writing %s: %w", part, err)
			}
			written += int64(n)
			j.done.Store(written)
		}
		if rerr == io.EOF {
			if written != total {
				return fmt.Errorf("incomplete transfer: %d bytes out of %d", written, total)
			}
			break
		}
		if rerr != nil {
			return fmt.Errorf("transfer interrupted at byte %d: %w", written, rerr)
		}
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("writing %s: %w", part, err)
	}
	if fi, err := f.Stat(); err != nil || fi.Size() != total {
		return fmt.Errorf("wrong final size for %s", part)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", part, err)
	}
	if err := root.Rename(part, dest); err != nil {
		return fmt.Errorf("putting %s in place: %w", dest, err)
	}
	t := time.Unix(0, mtime)
	if err := root.Chtimes(dest, t, t); err != nil {
		m.log.Warn("modification time not applied", "path", dest, "err", err)
	}
	return nil
}

// partPath is an item's partial file: hidden, specific to the item, in the
// destination folder. Naming it "<destination>.part" would let a peer
// overwrite an already received file that happens to have that name.
func partPath(it Item) string {
	return path.Join(path.Dir(it.DestPath), ".cercle-"+strconv.FormatInt(it.ID, 10)+".part")
}

const (
	minFree        = 1 << 30   // free space always left on the receiving disk
	diskCheckEvery = 256 << 20 // free space check during a transfer
)

// parseContentRange reads "bytes <start>-<end>/<size>".
func parseContentRange(s string) (start, size int64, err error) {
	rest, ok := strings.CutPrefix(s, "bytes ")
	rng, sz, ok2 := strings.Cut(rest, "/")
	a, _, ok3 := strings.Cut(rng, "-")
	if !ok || !ok2 || !ok3 {
		return 0, 0, fmt.Errorf("unreadable Content-Range: %q", s)
	}
	if start, err = strconv.ParseInt(a, 10, 64); err != nil {
		return 0, 0, err
	}
	if size, err = strconv.ParseInt(sz, 10, 64); err != nil {
		return 0, 0, err
	}
	return start, size, nil
}
