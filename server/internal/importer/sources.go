package importer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/storage"
)

// Sources (#99, #95): a collection already in a folder on the server, a NAS mounted there, or a
// OneDrive, imported where it is. A scan walks the source and compares it with what the last one
// saw (source_files): new files are imported (a batch of kind "source"), changed ones again; a file
// that moved is followed by its content; files gone mark the library's songs missing, and back
// restore them. When too many are gone at once the source is more likely not all there (a NAS
// half mounted, a share that lost its permissions) than emptied: nothing is marked, and the scan
// says so. Nothing in a source is ever written, moved or deleted.

// SourceStores is what scanning a source needs; *storage.Set is one.
type SourceStores interface {
	Backend(ctx context.Context, id int64) (storage.Backend, *storage.Store, error)
	Check(ctx context.Context, id int64) (*storage.Store, error)
	SetScan(ctx context.Context, id int64, v any) error
}

// ScanResult is what a scan of a source found and did.
type ScanResult struct {
	At      int64 `json:"at"`
	Files   int   `json:"files"`            // music, CUE sheets, logs, archives and pictures there
	New     int   `json:"new"`              // queued to be imported
	Changed int   `json:"changed"`          // other content than before: queued again, the old song missing until it is
	Moved   int   `json:"moved"`            // followed to where they are now
	Gone    int   `json:"gone"`             // songs newly missing
	Back    int   `json:"back"`             // missing songs whose files are back
	Covers  int   `json:"covers,omitempty"` // pictures taken as covers that moved, have other bytes, or are gone: followed (review #221)
	Bytes   int64 `json:"bytes"`            // the size of what is queued: what the import reads
	Batch   int64 `json:"batch,omitempty"`
	// Gallery pictures newly missing, and missing ones back (review #244).
	PicturesGone int `json:"pictures_gone,omitempty"`
	PicturesBack int `json:"pictures_back,omitempty"`
	// Music files gone, and pictures, but too many at once: none marked.
	Held         int    `json:"held,omitempty"`
	HeldPictures int    `json:"held_pictures,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ErrScanning is a scan asked for while one of the same source runs.
var ErrScanning = errors.New("this source is being scanned already")

// heldMin is how many gone music files are always believed; more than half the source's gone at
// once beyond that is not. Pictures do not count: a booklet's scans may outnumber its songs, and are
// moved out more readily (review #244).
const heldMin = 20

type sourceRow struct {
	key, path, version, sha, state string
	size, goneAt                   int64
}

// importable is what a scan tracks: what an import takes (lyrics are read next to the music, and
// are not imports of their own; pictures are both: an album's cover and its gallery).
func importable(p string) bool {
	ext := strings.ToLower(path.Ext(p))
	return audioExt[ext] || sidecarExt[ext] || bookletExt[ext] || ext == ".zip"
}

// music is a file a source's songs are imported from: audio, or an archive of it.
func music(p string) bool {
	ext := strings.ToLower(path.Ext(p))
	return audioExt[ext] || ext == ".zip"
}

func roleFor(p string) string {
	ext := strings.ToLower(path.Ext(p))
	switch {
	case sidecarExt[ext]:
		return RoleSidecar
	case bookletExt[ext]:
		return RoleImage
	case ext == ".zip":
		return RoleZip
	}
	return RoleAudio
}

// Scanning says whether a source is being scanned now, asked for or on schedule.
func (im *Importer) Scanning(id int64) bool {
	im.mu.Lock()
	defer im.mu.Unlock()
	return im.scanning[id]
}

// ScanSource scans source id: see the top of this file. preview has new files wait for the user
// after analysis, as other imports can.
func (im *Importer) ScanSource(ctx context.Context, stores SourceStores, id int64, preview bool) (res ScanResult, err error) {
	im.mu.Lock()
	if im.scanning[id] {
		im.mu.Unlock()
		return res, ErrScanning
	}
	im.scanning[id] = true
	im.mu.Unlock()
	defer func() {
		im.mu.Lock()
		delete(im.scanning, id)
		im.mu.Unlock()
	}()
	res.At = db.Now()
	defer func() {
		if err != nil {
			res.Error = err.Error()
		}
		if e := stores.SetScan(context.WithoutCancel(ctx), id, res); e != nil {
			im.log.Warn("record scan", "store", id, "err", e)
		}
	}()
	b, st, err := stores.Backend(ctx, id)
	if err != nil {
		return res, err
	}
	if st.Mode != storage.Source {
		return res, errors.New("not a source: a managed location is checked, not scanned")
	}
	if now, err := stores.Check(ctx, id); err != nil {
		return res, err
	} else if now.State != storage.StateOK {
		return res, fmt.Errorf("%w: %s", storage.ErrOffline, now.Error)
	}
	root := ""
	if r, ok := b.(interface{ Root() string }); ok {
		root = r.Root()
	}
	walked := map[string]storage.Object{}
	beside := besides{}                     // a cloud source's pictures and lyrics, fetched with its songs
	pictures := map[string]storage.Object{} // its pictures: those taken as covers are followed (review #221)
	err = b.Walk(ctx, func(o storage.Object) error {
		if importable(o.Path) {
			walked[o.Key] = o
			if !isImage(o.Path) {
				return nil
			}
		}
		if imageExt[strings.ToLower(path.Ext(o.Path))] && o.Size <= maxCoverFile {
			pictures[o.Key] = o
		}
		if root == "" {
			beside.add(o)
		}
		return nil
	})
	var partial *storage.PartialError
	if errors.As(err, &partial) {
		err = nil
	}
	if err != nil {
		return res, err
	}
	res.Files = len(walked)
	rows, err := im.sourceRows(ctx, id)
	if err != nil {
		return res, err
	}
	ref := func(key string) string { return storage.Ref(st.Kind, id, key) }

	var fresh, changed []storage.Object
	var back []string
	for key, o := range walked {
		r, ok := rows[key]
		switch {
		case !ok:
			fresh = append(fresh, o)
		case r.size == o.Size && r.version == o.Version:
			if r.goneAt != 0 {
				back = append(back, key)
			}
			if r.path != o.Path { // renamed or moved in a OneDrive, whose keys stay
				if _, err := im.db.ExecContext(ctx, `UPDATE source_files SET path = ? WHERE store_id = ? AND key = ?`, o.Path, id, key); err != nil {
					return res, err
				}
			}
		default:
			changed = append(changed, o)
		}
	}
	// What the walk did not see: gone, or moved (a file with its content is among the new ones).
	unseen := map[int64][]*sourceRow{}
	var imported int // music files: what a source not all there is told by
	for key, r := range rows {
		if r.state == "imported" && music(r.path) {
			imported++
		}
		if _, ok := walked[key]; ok || (partial != nil && partial.Covers(r.path)) {
			continue
		}
		unseen[r.size] = append(unseen[r.size], r)
	}
	var queue []storage.Object
	for _, o := range fresh {
		if moved, err := im.followMove(ctx, b, unseen, o, ref); err != nil {
			return res, err
		} else if moved {
			res.Moved++
			continue
		}
		queue = append(queue, o)
	}
	res.New = len(queue)

	// Gone: the songs imported from there are missing. Files that never made a song are forgotten,
	// but for those the library let go of: they stay let go of should they come back.
	var gone, forget []string
	goneMusic, gonePictures := 0, 0
	for _, list := range unseen {
		for _, r := range list {
			switch {
			case r.state == "imported" && r.goneAt == 0:
				gone = append(gone, r.key)
				if music(r.path) {
					goneMusic++
				} else if isImage(r.path) {
					gonePictures++
				}
			case r.state != "imported" && r.state != "ignored":
				forget = append(forget, r.key)
			}
		}
	}
	// So many music files gone at once is more likely a source not all there (a NAS partly mounted)
	// than files deleted: none of what was not seen is taken for gone, pictures and forgotten files
	// included (review #227); nothing there at all is not either. Pictures gone alone are believed
	// (review #244).
	held := goneMusic > heldMin && goneMusic*2 > imported || len(walked) == 0 && len(gone) > heldMin
	if held {
		res.Held, res.HeldPictures = goneMusic, gonePictures
		im.log.Warn("source scan: too many files gone at once; none marked missing", "store", id, "gone", goneMusic, "of", imported,
			"pictures", gonePictures)
		forget = nil
	} else if len(gone) > 0 {
		refs := make([]string, len(gone))
		for i, k := range gone {
			refs[i] = ref(k)
		}
		if res.Gone, err = im.lib.MarkRefsMissing(ctx, refs); err != nil {
			return res, err
		}
		if res.PicturesGone, err = im.lib.SetImagesMissing(ctx, refs, true); err != nil { // gallery pictures too (review #238)
			return res, err
		}
		if err := im.setGone(ctx, id, gone, db.Now()); err != nil {
			return res, err
		}
	}
	for _, key := range forget {
		if _, err := im.db.ExecContext(ctx, `DELETE FROM source_files WHERE store_id = ? AND key = ?`, id, key); err != nil {
			return res, err
		}
	}
	if len(back) > 0 {
		refs := make([]string, len(back))
		for i, k := range back {
			refs[i] = ref(k)
		}
		if res.Back, err = im.lib.RestoreRefs(ctx, refs); err != nil {
			return res, err
		}
		if res.PicturesBack, err = im.lib.SetImagesMissing(ctx, refs, false); err != nil {
			return res, err
		}
		if err := im.setGone(ctx, id, back, 0); err != nil {
			return res, err
		}
	}
	// Changed: the song there has other bytes now; it is missing until the new ones are imported
	// (the same song again when only the time changed). A gallery's picture alike: the new bytes
	// take its place in the gallery as another picture (review #238).
	if len(changed) > 0 {
		refs := make([]string, len(changed))
		for i, o := range changed {
			refs[i] = ref(o.Key)
		}
		if _, err := im.lib.MarkRefsMissing(ctx, refs); err != nil {
			return res, err
		}
		if _, err := im.lib.SetImagesMissing(ctx, refs, true); err != nil {
			return res, err
		}
		res.Changed = len(changed)
		queue = append(queue, changed...)
	}
	if res.Covers, err = im.followCovers(ctx, b, st, pictures, partial, held); err != nil {
		return res, err
	}
	if len(queue) == 0 {
		return res, nil
	}
	for _, o := range queue {
		res.Bytes += o.Size
	}
	res.Batch, err = im.queueSource(ctx, st, root, queue, beside, preview)
	return res, err
}

func (im *Importer) sourceRows(ctx context.Context, id int64) (map[string]*sourceRow, error) {
	rows, err := im.db.QueryContext(ctx, `SELECT key, path, size, version, sha256, state, gone_at FROM source_files WHERE store_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*sourceRow{}
	for rows.Next() {
		var r sourceRow
		if err := rows.Scan(&r.key, &r.path, &r.size, &r.version, &r.sha, &r.state, &r.goneAt); err != nil {
			return nil, err
		}
		out[r.key] = &r
	}
	return out, rows.Err()
}

func (im *Importer) setGone(ctx context.Context, id int64, keys []string, at int64) error {
	tx, err := im.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, k := range keys {
		if _, err := tx.ExecContext(ctx, `UPDATE source_files SET gone_at = ? WHERE store_id = ? AND key = ?`, at, id, k); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// followMove takes o, a file the last scan did not see, for one that scan saw elsewhere and this
// one did not, when it has that one's content: the library's song is read from o's place from now.
func (im *Importer) followMove(ctx context.Context, b storage.Backend, unseen map[int64][]*sourceRow, o storage.Object,
	ref func(string) string) (bool, error) {
	var hashed string
	for i, r := range unseen[o.Size] {
		if r.state != "imported" || r.sha == "" {
			continue
		}
		if hashed == "" {
			sha, err := hashObject(ctx, b, o)
			if err != nil {
				im.log.Warn("source scan: read a new file", "file", o.Path, "err", err)
				return false, nil // imported as a new file
			}
			hashed = sha
		}
		if r.sha != hashed {
			continue
		}
		if err := im.lib.MoveRef(ctx, ref(r.key), ref(o.Key)); err != nil {
			return false, err
		}
		tx, err := im.db.BeginTx(ctx, nil)
		if err != nil {
			return false, err
		}
		defer tx.Rollback()
		_, storeID, _, _ := storage.Parse(ref(o.Key))
		if _, err := tx.ExecContext(ctx, `DELETE FROM source_files WHERE store_id = ? AND key = ?`, storeID, r.key); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_files (store_id, key, path, size, version, sha256, state, seen_at)
			VALUES (?, ?, ?, ?, ?, ?, 'imported', ?)`, storeID, o.Key, o.Path, o.Size, o.Version, hashed, db.Now()); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		list := unseen[o.Size]
		unseen[o.Size] = append(list[:i:i], list[i+1:]...)
		return true, nil
	}
	return false, nil
}

func hashObject(ctx context.Context, b storage.Backend, o storage.Object) (string, error) {
	rc, err := b.Open(ctx, o.Key, 0, -1)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		return "", err
	}
	if n != o.Size {
		return "", io.ErrUnexpectedEOF
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// queueSource queues a source's files as one batch, imported where they are: a folder's files by
// their paths there, a cloud's fetched when their turn comes (with what is beside them).
func (im *Importer) queueSource(ctx context.Context, st *storage.Store, root string, files []storage.Object, beside besides,
	preview bool) (int64, error) {
	tx, err := im.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := db.Now()
	opts, _ := json.Marshal(batchOptions{Store: st.ID})
	r, err := tx.ExecContext(ctx, `INSERT INTO import_batches (kind, source, state, preview, root, options, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"source", st.Name, BatchAnalyzing, preview, root, string(opts), now)
	if err != nil {
		return 0, err
	}
	batchID, _ := r.LastInsertId()
	for _, o := range files {
		local, fetch := "", ""
		switch {
		case root != "":
			local = filepath.Join(root, filepath.FromSlash(o.Key))
		case isImage(o.Path):
			fetch = besides{}.fetchFor(o) // a picture alone, without what is beside it
		default:
			fetch = beside.fetchFor(o)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO import_items (batch_id, local_path, rel_path, state, role, ref, drive_size, fetch, updated_at)
			VALUES (?, ?, ?, 'pending', ?, ?, ?, ?, ?)`, batchID, local, o.Path, roleFor(o.Path), storage.Ref(st.Kind, st.ID, o.Key), o.Size,
			fetch, now); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_files (store_id, key, path, size, version, state, seen_at)
			VALUES (?, ?, ?, ?, ?, 'queued', ?) ON CONFLICT (store_id, key) DO UPDATE SET path = excluded.path, size = excluded.size,
			version = excluded.version, sha256 = '', state = 'queued', seen_at = excluded.seen_at, gone_at = 0`,
			st.ID, o.Key, o.Path, o.Size, o.Version, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	im.Wake()
	return batchID, nil
}

// sourceCategory puts the album a source's song went into in the category the source's albums go
// in (#92), as it is set now.
func (im *Importer) sourceCategory(ctx context.Context, batchID, entryID int64) {
	if entryID == 0 {
		return
	}
	var category, album int64
	err := im.db.QueryRowContext(ctx, `SELECT s.category, e.album_id FROM import_batches b
		JOIN stores s ON s.id = json_extract(b.options, '$.store') JOIN album_entries e ON e.id = ?
		WHERE b.id = ? AND b.kind = 'source' AND s.category IS NOT NULL`, entryID, batchID).Scan(&category, &album)
	if err == nil {
		err = im.lib.AddImported(ctx, album, category)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		im.log.Warn("put a source's album in its category", "batch", batchID, "err", err)
	}
}

// noteSource records what became of a source's file once its import is recorded.
func (im *Importer) noteSource(ctx context.Context, it *item, out outcome) {
	_, id, key, ok := storage.Parse(it.ref)
	if !ok {
		return
	}
	state := map[string]string{StatePublished: "imported", StateDuplicate: "imported", StateSkipped: "skipped", StateFailed: "failed"}[out.state]
	if state == "" {
		return
	}
	if _, err := im.db.ExecContext(ctx, `UPDATE source_files SET state = ?, sha256 = CASE WHEN ?2 = '' THEN sha256 ELSE ?2 END
		WHERE store_id = ? AND key = ?`, state, out.sha, id, key); err != nil {
		im.log.Warn("record source file", "ref", it.ref, "err", err)
	}
}

// itemContext is the context a file is imported in: a source's file stops when its source is
// removed (RemoveSource). removed: the source is being removed, the file is not imported.
func (im *Importer) itemContext(ctx context.Context, it *item) (ictx context.Context, done func(), removed bool) {
	_, store, _, ok := storage.Parse(it.ref)
	if !ok {
		return ctx, func() {}, false
	}
	im.mu.Lock()
	defer im.mu.Unlock()
	if im.removing[store] {
		return ctx, func() {}, true
	}
	ictx, cancel := context.WithCancel(ctx)
	im.stopItem[store] = cancel
	return ictx, func() {
		cancel()
		im.mu.Lock()
		delete(im.stopItem, store)
		im.mu.Unlock()
	}, false
}

// RemoveSource removes source id (its references starting with prefix) with remove, which lets go of
// the library's songs from there and deletes the store in tx. Nothing of it is left to import after
// (review #222): a scan of it running refuses it (ErrScanning) and none starts meanwhile; the file of
// it being imported is stopped and recorded first; and in the same transaction its batches waiting
// for review or being analyzed are cancelled, and its files not imported yet in others left out.
func (im *Importer) RemoveSource(ctx context.Context, id int64, prefix string, remove func(tx *sql.Tx) error) error {
	im.mu.Lock()
	if im.scanning[id] {
		im.mu.Unlock()
		return ErrScanning
	}
	im.scanning[id], im.removing[id] = true, true
	if stop := im.stopItem[id]; stop != nil {
		stop()
	}
	im.mu.Unlock()
	defer func() {
		im.mu.Lock()
		delete(im.scanning, id)
		delete(im.removing, id)
		im.mu.Unlock()
	}()
	for { // the file stopped is recorded before its source goes
		im.mu.Lock()
		_, busy := im.stopItem[id]
		im.mu.Unlock()
		if !busy {
			break
		}
		if !sleepCtx(ctx, 20*time.Millisecond) {
			return ctx.Err()
		}
	}
	from, to := prefix, prefix[:len(prefix)-1]+";"
	tx, err := im.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ids := func(q string, args ...any) ([]int64, error) {
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			out = append(out, id)
		}
		return out, rows.Err()
	}
	canceled, err := ids(`SELECT id FROM import_batches WHERE kind = 'source' AND json_extract(options, '$.store') = ? AND state IN (?, ?)`,
		id, BatchReview, BatchAnalyzing)
	if err != nil {
		return err
	}
	running, err := ids(`SELECT DISTINCT batch_id FROM import_items WHERE ref >= ? AND ref < ? AND state IN ('pending', 'uploading')`, from, to)
	if err != nil {
		return err
	}
	now := db.Now()
	for _, b := range canceled {
		if _, err := tx.ExecContext(ctx, `UPDATE import_batches SET state = ?, finished_at = ? WHERE id = ?`, BatchCanceled, now, b); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE import_items SET state = ?, error = ?, updated_at = ? WHERE batch_id = ? AND state = 'pending'`,
			StateExcluded, library.ErrStoreGone.Error(), now, b); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE import_items SET state = ?, error = ?, updated_at = ? WHERE ref >= ? AND ref < ?
		AND state IN ('pending', 'uploading')`, StateExcluded, library.ErrStoreGone.Error(), now, from, to); err != nil {
		return err
	}
	if err := remove(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, b := range canceled {
		os.RemoveAll(im.workDir(b))
		var source string
		im.db.QueryRowContext(ctx, `SELECT source FROM import_batches WHERE id = ?`, b).Scan(&source)
		if im.OnBatchDone != nil {
			im.OnBatchDone(ctx, "source", source, 0)
		}
	}
	for _, b := range running {
		im.finishIfIdle(ctx, b) // what is left of it may be done now
	}
	return nil
}

// settleSource records what became of a source batch's files that no import recorded (review #228):
// those left out in review, and those skipped or failed when analyzed. A batch canceled leaves its
// files unknown, for the next scan to find again; one imported remembers the files left out of it
// as let go of (as songs deleted from the library: not imported again until they change). A file
// queued again by a later scan is that one's.
func (im *Importer) settleSource(ctx context.Context, batchID int64, canceled bool) {
	rows, err := im.db.QueryContext(ctx, `SELECT ref, state FROM import_items WHERE batch_id = ? AND ref != ''`, batchID)
	if err != nil {
		im.log.Warn("settle a source's files", "batch", batchID, "err", err)
		return
	}
	type file struct{ ref, state string }
	var files []file
	for rows.Next() {
		var f file
		if rows.Scan(&f.ref, &f.state) == nil {
			files = append(files, f)
		}
	}
	rows.Close()
	became := map[string]string{StateExcluded: "ignored", StateSkipped: "skipped", StateFailed: "failed", StateDiscarded: "failed",
		StatePublished: "imported", StateDuplicate: "imported"}
	for _, f := range files {
		_, store, key, ok := storage.Parse(f.ref)
		state := became[f.state]
		if !ok || (state == "" && !canceled) {
			continue
		}
		mine := ` AND state = 'queued' AND NOT EXISTS (SELECT 1 FROM import_items i JOIN import_batches b ON b.id = i.batch_id
			WHERE i.ref = ? AND i.batch_id != ? AND b.state NOT IN (?, ?))`
		args := []any{store, key, f.ref, batchID, BatchDone, BatchCanceled}
		var err error
		if canceled {
			_, err = im.db.ExecContext(ctx, `DELETE FROM source_files WHERE store_id = ? AND key = ?`+mine, args...)
		} else {
			_, err = im.db.ExecContext(ctx, `UPDATE source_files SET state = ? WHERE store_id = ? AND key = ?`+mine, append([]any{state}, args...)...)
		}
		if err != nil {
			im.log.Warn("settle a source's file", "ref", f.ref, "err", err)
		}
	}
}

// ForgetSource leaves a source's file the library let go of out of the scans until it changes.
func (im *Importer) ForgetSource(ctx context.Context, store int64, key string) error {
	_, err := im.db.ExecContext(ctx, `UPDATE source_files SET state = 'ignored' WHERE store_id = ? AND key = ?`, store, key)
	return err
}

// unchangedLocal checks that a source's file on the server is still what was read: hashing reads
// it where it is, and it may have been replaced or written to meanwhile.
func unchangedLocal(p string, before os.FileInfo) error {
	after, err := os.Stat(p)
	if err != nil {
		return err
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return errors.New("the file changed while it was being read; it is imported again on the next scan")
	}
	return nil
}
