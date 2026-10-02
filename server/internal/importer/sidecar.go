package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/library"
)

const maxSidecar = 16 << 20

// processSidecar keeps a CUE sheet or rip log in Drive under sidecars/, attached to the album
// imported from the same folder (decision D2 §5). It runs after the batch's audio.
func (im *Importer) processSidecar(ctx context.Context, it *item) (outcome, error) {
	var out outcome
	f, err := os.Open(it.path)
	if err != nil {
		return out, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return out, err
	}
	if st.Size() > maxSidecar {
		f.Close()
		out.state, out.msg = StateSkipped, "larger than 16 MB: not a CUE sheet or rip log"
		return out, nil
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return out, err
	}
	out.sha = hex.EncodeToString(h.Sum(nil))

	var albumID int64
	folder := albumRoot(path.Dir(it.rel))
	im.db.QueryRowContext(ctx, `SELECT e.album_id FROM import_items i JOIN album_entries e ON e.id = i.entry_id
		WHERE i.batch_id = ? AND i.role = ? AND json_extract(i.plan, '$.folder') = ? ORDER BY i.id LIMIT 1`,
		it.batchID, RoleAudio, folder).Scan(&albumID)
	if albumID == 0 {
		out.state, out.msg = StateSkipped, "no album was imported from this folder"
		return out, nil
	}
	if c, err := im.lib.FindSidecar(ctx, albumID, out.sha, st.Size()); err != nil || c != nil {
		out.state = StateDuplicate
		return out, err
	}
	album, err := im.lib.Album(ctx, albumID)
	if err != nil {
		return out, err
	}
	if album == nil {
		return out, errors.New("the album is gone")
	}
	dir := "sidecars/" + strings.TrimPrefix(drivePath(library.EntryInput{Album: album.Title, AlbumArtist: album.AlbumArtist}), "library/")
	parent, err := im.drive.Folder(ctx, dir)
	if err != nil {
		return out, err
	}
	name := filepath.Base(it.path)
	var file gdrive.File
	if df, ok := im.df(); ok && it.driveID != "" { // from the Drive inbox: move it, do not upload a copy
		if err := df.Move(ctx, it.driveID, parent, []string{it.driveParent}); err != nil {
			return out, err
		}
		file.ID = it.driveID
	} else if file, err = im.drive.Upload(ctx, gdrive.Upload{Path: it.path, Name: name, ParentID: parent, MIME: "text/plain",
		Size: st.Size(), SHA256: out.sha, Sessions: &memSessions{}}); err != nil {
		return out, err
	}
	kind := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	if _, err := im.lib.AddSidecar(ctx, albumID, name, kind, out.sha, st.Size(), file.ID); err != nil {
		return out, err
	}
	out.state = StatePublished
	return out, nil
}
