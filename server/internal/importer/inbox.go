package importer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/media"
)

// The Drive inbox (decision D6). Files dropped into Kanade/inbox, for example copied there from
// another cloud by OpenList, are imported where they are: audio is read through Range requests
// and, already being in Drive, moved into library/ on the Drive side instead of uploaded; Drive's
// own SHA-256 finds files the library has. Files that need FFmpeg, CUE sheets and logs, and ZIPs
// are fetched to staging first and then take the usual way. Nothing in the inbox is deleted:
// duplicates are moved to inbox/重複, and what a finished batch leaves behind (originals that were
// converted, covers, lyrics, skipped files) to inbox/已處理.

const (
	InboxFolder     = "inbox"
	inboxDuplicates = "重複"
	inboxProcessed  = "已處理"
	// defaultInboxSettle is how long a top-level folder must go without new files before it is
	// imported, so an album still being copied in is not split across batches (SetInboxSettle).
	defaultInboxSettle = 5 * time.Minute
	// At most this many new files are looked at per scan and queued per batch.
	inboxScanMax  = 20000
	inboxBatchMax = 2000
)

// driveFiles is what the inbox needs from Drive beyond uploading; *gdrive.Client has it.
type driveFiles interface {
	Children(ctx context.Context, folder string) ([]gdrive.File, error)
	Move(ctx context.Context, id, to string, from []string) error
	OpenRange(ctx context.Context, id string, start, end int64) (*http.Response, error)
	GetFile(ctx context.Context, id, fields string) (gdrive.File, error)
}

// SetInboxSettle changes how long an inbox folder must go without new files (review #77).
func (im *Importer) SetInboxSettle(d time.Duration) {
	if d <= 0 {
		d = -1 // no wait (0 is "not set")
	}
	im.settle.Store(int64(d))
}

func (im *Importer) inboxSettle() time.Duration {
	switch d := im.settle.Load(); {
	case d > 0:
		return time.Duration(d)
	case d < 0:
		return 0
	}
	return defaultInboxSettle
}

func (im *Importer) df() (driveFiles, bool) {
	d, ok := im.drive.(driveFiles)
	return d, ok
}

// ScanInbox queues the inbox's new files as one batch and says how many there were. Files under
// a top-level entry that changed in the last few minutes wait for the next scan (InboxWaiting).
func (im *Importer) ScanInbox(ctx context.Context) (int, error) {
	df, ok := im.df()
	if !ok {
		return 0, nil
	}
	root, err := im.drive.Folder(ctx, InboxFolder)
	if err != nil {
		return 0, err
	}
	known := map[string]bool{}
	rows, err := im.db.QueryContext(ctx, `SELECT drive_id FROM import_items WHERE drive_id != ''`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		known[id] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	type found struct {
		f           gdrive.File
		rel, parent string
	}
	// Only files that can be imported and are new count toward the limits, so covers, scans and
	// files already handled never crowd out the rest; what is over the batch limit waits for the
	// next scan.
	var files []found
	newest := map[string]time.Time{} // per top-level folder, or loose file
	var walk func(folder, prefix string, depth int) error
	walk = func(folder, prefix string, depth int) error {
		list, err := df.Children(ctx, folder)
		if err != nil {
			return err
		}
		for _, f := range list {
			if f.MimeType == gdrive.FolderMime {
				if (prefix == "" && (f.Name == inboxDuplicates || f.Name == inboxProcessed)) || depth >= 8 {
					continue
				}
				if err := walk(f.ID, prefix+f.Name+"/", depth+1); err != nil {
					return err
				}
				continue
			}
			rel := prefix + f.Name
			top, _, _ := strings.Cut(rel, "/")
			if t, err := time.Parse(time.RFC3339, f.CreatedTime); err == nil && t.After(newest[top]) {
				newest[top] = t
			}
			if !known[f.ID] && roleOf(rel) != "" && len(files) < inboxScanMax {
				files = append(files, found{f, rel, folder})
			}
		}
		return nil
	}
	if err := walk(root, "", 0); err != nil {
		return 0, err
	}
	var fresh []found
	waiting := 0
	for _, f := range files {
		if top, _, _ := strings.Cut(f.rel, "/"); time.Since(newest[top]) < im.inboxSettle() {
			waiting++
		} else if len(fresh) < inboxBatchMax {
			fresh = append(fresh, f)
		}
	}
	im.inboxWaiting.Store(int64(waiting))
	if len(fresh) == 0 {
		return 0, nil
	}
	tx, err := im.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := db.Now()
	r, err := tx.ExecContext(ctx, `INSERT INTO import_batches (kind, source, state, preview, created_at) VALUES ('inbox', ?, ?, 0, ?)`,
		"Google Drive "+InboxFolder, BatchAnalyzing, now)
	if err != nil {
		return 0, err
	}
	batch, _ := r.LastInsertId()
	for _, f := range fresh {
		if _, err := tx.ExecContext(ctx, `INSERT INTO import_items (batch_id, local_path, rel_path, state, role, sha256, drive_id,
			drive_parent, drive_size, updated_at) VALUES (?, '', ?, 'pending', ?, ?, ?, ?, ?, ?)`,
			batch, f.rel, roleOf(f.rel), strings.ToLower(f.f.SHA256Checksum), f.f.ID, f.parent, f.f.SizeBytes(), now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	im.Wake()
	return len(fresh), nil
}

// InboxWaiting is how many new inbox files the last scan left for later, as they were still
// arriving.
func (im *Importer) InboxWaiting() int { return int(im.inboxWaiting.Load()) }

// tidyInbox moves what a finished inbox batch leaves behind into inbox/已處理: each top-level
// folder whose importable files have all been imported, set aside or skipped, and the loose files
// of the batch still in the inbox. Folders with failed files, or files no batch has handled yet,
// stay where they are. Nothing is deleted.
func (im *Importer) tidyInbox(ctx context.Context, batchID int64) error {
	df, ok := im.df()
	if !ok {
		return nil
	}
	rows, err := im.db.QueryContext(ctx, `SELECT rel_path, drive_id, state FROM import_items WHERE batch_id = ? AND drive_id != ''`, batchID)
	if err != nil {
		return err
	}
	tops, loose := map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var rel, id, state string
		if err := rows.Scan(&rel, &id, &state); err != nil {
			rows.Close()
			return err
		}
		if top, _, nested := strings.Cut(rel, "/"); nested {
			tops[top] = true
		} else if inboxFinal[state] {
			loose[id] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil || (len(tops) == 0 && len(loose) == 0) {
		return err
	}
	root, err := im.drive.Folder(ctx, InboxFolder)
	if err != nil {
		return err
	}
	list, err := df.Children(ctx, root)
	if err != nil {
		return err
	}
	dest := ""
	for _, f := range list {
		if f.MimeType == gdrive.FolderMime {
			if !tops[f.Name] || f.Name == inboxDuplicates || f.Name == inboxProcessed {
				continue
			}
			if done, err := im.inboxFinished(ctx, df, f.ID, 0); err != nil || !done {
				if err != nil {
					return err
				}
				continue
			}
		} else if !loose[f.ID] {
			continue
		}
		if dest == "" {
			if dest, err = im.drive.Folder(ctx, InboxFolder+"/"+inboxProcessed); err != nil {
				return err
			}
		}
		if err := df.Move(ctx, f.ID, dest, []string{root}); err != nil {
			return err
		}
	}
	return nil
}

// inboxFinal are the item states after which an inbox file needs nothing more.
var inboxFinal = map[string]bool{StatePublished: true, StateDuplicate: true, StateSkipped: true,
	StateExcluded: true, StateExpanded: true, StateSplit: true}

// inboxFinished says whether every importable file below an inbox folder has been handled.
func (im *Importer) inboxFinished(ctx context.Context, df driveFiles, folder string, depth int) (bool, error) {
	if depth >= 8 {
		return false, nil
	}
	list, err := df.Children(ctx, folder)
	if err != nil {
		return false, err
	}
	for _, f := range list {
		if f.MimeType == gdrive.FolderMime {
			if done, err := im.inboxFinished(ctx, df, f.ID, depth+1); err != nil || !done {
				return false, err
			}
			continue
		}
		if roleOf(f.Name) == "" {
			continue
		}
		var state string
		err := im.db.QueryRowContext(ctx, `SELECT state FROM import_items WHERE drive_id = ? ORDER BY id DESC LIMIT 1`, f.ID).Scan(&state)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		if !inboxFinal[state] {
			return false, nil
		}
	}
	return true, nil
}

func roleOf(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch {
	case audioExt[ext]:
		return RoleAudio
	case sidecarExt[ext]:
		return RoleSidecar
	case ext == ".zip":
		return RoleZip
	}
	return ""
}

// fetchDrive copies an inbox file to staging/work, for what has to be local (FFmpeg, archives,
// sidecars), and records the copy as the item's local file.
func (im *Importer) fetchDrive(ctx context.Context, batchID, itemID int64, driveID, rel string, size int64) (string, error) {
	df, ok := im.df()
	if !ok {
		return "", errors.New("drive is not available")
	}
	release, err := im.hold(ctx, size)
	if err != nil {
		return "", fmt.Errorf("not enough staging space to fetch it from Drive: %w", err)
	}
	defer release()
	dir := filepath.Join(im.workDir(batchID), "inbox", strconv.FormatInt(itemID, 10))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, path.Base(rel))
	resp, err := df.OpenRange(ctx, driveID, 0, -1)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != size {
		err = fmt.Errorf("fetched %d of %d bytes", n, size)
	}
	if err != nil {
		os.Remove(dst)
		return "", err
	}
	_, err = im.db.ExecContext(ctx, `UPDATE import_items SET local_path = ?, temp = 1, updated_at = ? WHERE id = ?`, dst, db.Now(), itemID)
	return dst, err
}

// fetchInboxLocal fetches the batch's inbox sidecars and archives: they are small, or have to be
// unpacked.
func (im *Importer) fetchInboxLocal(ctx context.Context, batchID int64) error {
	rows, err := im.db.QueryContext(ctx, `SELECT id, drive_id, rel_path, drive_size FROM import_items
		WHERE batch_id = ? AND local_path = '' AND drive_id != '' AND role IN (?, ?) AND state = 'pending'`, batchID, RoleSidecar, RoleZip)
	if err != nil {
		return err
	}
	type item struct {
		id    int64
		drive string
		rel   string
		size  int64
	}
	var list []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.id, &it.drive, &it.rel, &it.size) == nil {
			list = append(list, it)
		}
	}
	rows.Close()
	for _, it := range list {
		if it.size > maxSidecar && roleOf(it.rel) == RoleSidecar {
			im.itemFailed(ctx, it.id, StateSkipped, "larger than 16 MB: not a CUE sheet or rip log")
			continue
		}
		if _, err := im.fetchDrive(ctx, batchID, it.id, it.drive, it.rel, it.size); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			im.itemFailed(ctx, it.id, StateFailed, "cannot fetch it from Drive: "+err.Error())
		}
	}
	return nil
}

// probeDrive reads an inbox file's tags in place.
func (im *Importer) probeDrive(ctx context.Context, driveID string, size int64) (*media.Info, error) {
	df, ok := im.df()
	if !ok {
		return nil, errors.New("drive is not available")
	}
	return media.Probe(gdrive.NewReaderAt(ctx, df, driveID, size), size)
}

// processDrive imports an inbox file without uploading it: the library takes the Drive file itself.
func (im *Importer) processDrive(ctx context.Context, it *item) (outcome, error) {
	var out outcome
	df, _ := im.df()
	// The file may have been replaced since the scan: take its identity now, and check it again
	// once its tags are read, so new bytes are never published under an old checksum.
	identity := func() (string, int64, []string, error) {
		f, err := df.GetFile(ctx, it.driveID, "id,size,parents,sha256Checksum")
		if err != nil {
			return "", 0, nil, err
		}
		sha := strings.ToLower(f.SHA256Checksum)
		if sha == "" {
			return "", 0, nil, errors.New("Drive has not computed this file's checksum yet; retry in a while")
		}
		return sha, f.SizeBytes(), f.Parents, nil
	}
	sha, size, parents, err := identity()
	if err != nil {
		return out, err
	}
	if sha != it.sha || size != it.driveSize {
		im.log.Info("inbox: file changed since the scan", "file", it.driveID)
		it.sha, it.driveSize = sha, size
		it.plan = nil // made from the old bytes' tags
		im.db.ExecContext(ctx, `UPDATE import_items SET sha256 = ?, drive_size = ? WHERE id = ?`, sha, size, it.id)
	}
	if len(parents) > 0 {
		it.driveParent = parents[0]
	}
	out.sha = sha
	info, err := im.probeDrive(ctx, it.driveID, it.driveSize)
	if errors.Is(err, media.ErrUnknownFormat) {
		out.state, out.msg = StateSkipped, "not a recognized audio file"
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("cannot read audio: %w", err)
	}
	out.info = info
	if !info.Playable {
		out.state, out.msg = StateSkipped, unplayable(info)
		return out, nil
	}
	if again, againSize, _, err := identity(); err != nil {
		return out, err
	} else if again != sha || againSize != size {
		return out, errors.New("the file changed in Drive while it was being read; retry to import the new version")
	}
	asset, err := im.lib.AssetByHash(ctx, sha, it.driveSize)
	if err != nil {
		return out, err
	}
	if asset == nil {
		if asset, err = im.lib.CreateAsset(ctx, library.Asset{SHA256: sha, Size: it.driveSize, Format: info.Format, Codec: info.Codec,
			SampleRate: info.SampleRate, BitDepth: info.BitDepth, Channels: info.Channels, DurationMS: info.DurationMS,
			Bitrate: info.Bitrate, AudioMD5: info.AudioMD5}); err != nil {
			return out, err
		}
	}
	out.assetID = asset.ID
	in := entryInput(it.rel, info)
	if it.plan != nil {
		in = it.plan.input()
		in.AlbumID = im.groupAlbum(ctx, it.batchID, it.plan.Group)
	}
	copyInLibrary := asset.State == library.AssetVerified && asset.DriveFileID != it.driveID
	moved := asset.State == library.AssetVerified && asset.DriveFileID == it.driveID // by an attempt that stopped after it
	if !copyInLibrary && !moved {
		folder, err := im.drive.Folder(ctx, drivePath(in))
		if err != nil {
			return out, err
		}
		if err := df.Move(ctx, it.driveID, folder, []string{it.driveParent}); err != nil {
			return out, fmt.Errorf("moving it into the library: %w", err)
		}
		if err := im.lib.MarkVerified(ctx, asset.ID, it.driveID); err != nil {
			return out, err
		}
	}
	if in.Album != "" {
		in.CoverID = im.driveCover(ctx, it, info)
	}
	res, err := im.lib.Publish(ctx, asset.ID, in)
	if err != nil {
		return out, err
	}
	out.trackID, out.entry = res.TrackID, res.EntryID
	im.driveLyrics(ctx, it, res.TrackID, info)
	out.state = StatePublished
	if !res.Created {
		out.state = StateDuplicate
	}
	if copyInLibrary { // the library has these bytes in another file: set this one aside, not deleted
		if dup, err := im.drive.Folder(ctx, InboxFolder+"/"+inboxDuplicates); err == nil {
			if err := df.Move(ctx, it.driveID, dup, []string{it.driveParent}); err != nil {
				im.log.Warn("inbox: moving a duplicate aside", "file", it.driveID, "err", err)
			}
		}
	}
	return out, nil
}

// siblings lists an inbox folder once per run.
func (im *Importer) siblings(ctx context.Context, folder string) []gdrive.File {
	im.mu.Lock()
	list, ok := im.driveDirs[folder]
	im.mu.Unlock()
	if ok {
		return list
	}
	df, _ := im.df()
	list, err := df.Children(ctx, folder)
	if err != nil {
		im.log.Warn("inbox: listing a folder", "folder", folder, "err", err)
		return nil
	}
	im.mu.Lock()
	im.driveDirs[folder] = list
	im.mu.Unlock()
	return list
}

func (im *Importer) readDrive(ctx context.Context, f gdrive.File, limit int64) ([]byte, error) {
	if f.SizeBytes() > limit {
		return nil, errors.New("too large")
	}
	df, _ := im.df()
	resp, err := df.OpenRange(ctx, f.ID, 0, -1)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// driveCover picks album art for an inbox file the way coverFor does for local files: embedded,
// then a cover/folder/front/jacket image in its folder (or the album folder above a Disc folder),
// then one that looks like a cover, then the first image.
func (im *Importer) driveCover(ctx context.Context, it *item, info *media.Info) int64 {
	if info.Cover != nil {
		if id := im.storeCover(ctx, info.Cover.Data); id != 0 {
			return id
		}
	}
	key := "drive:" + it.driveParent
	im.mu.Lock()
	id, seen := im.covers[key]
	im.mu.Unlock()
	if seen {
		return id
	}
	folders := []string{it.driveParent}
	if discDir.MatchString(path.Base(path.Dir(it.rel))) {
		if df, ok := im.df(); ok {
			if f, err := df.GetFile(ctx, it.driveParent, "id,parents"); err == nil && len(f.Parents) > 0 {
				folders = append(folders, f.Parents[0])
			}
		}
	}
	var pick *gdrive.File
	for _, match := range []func(string) bool{
		func(s string) bool { return coverName.MatchString(s) },
		func(s string) bool { return coverLike.MatchString(s) },
		func(string) bool { return true },
	} {
		for _, folder := range folders {
			for _, f := range im.siblings(ctx, folder) {
				if imageExt[strings.ToLower(path.Ext(f.Name))] && match(stem(f.Name)) && pick == nil {
					f := f
					pick = &f
				}
			}
		}
		if pick != nil {
			break
		}
	}
	if pick != nil {
		if data, err := im.readDrive(ctx, *pick, maxCoverFile); err == nil {
			id = im.storeCover(ctx, data)
		}
	}
	im.mu.Lock()
	im.covers[key] = id
	im.mu.Unlock()
	return id
}

// driveLyrics keeps embedded lyrics, then a same-named .lrc in the inbox folder.
func (im *Importer) driveLyrics(ctx context.Context, it *item, trackID int64, info *media.Info) {
	if info.Tags.Lyrics != "" {
		im.lib.SetLyrics(ctx, trackID, library.LyricsEmbedded, info.Tags.Lyrics)
	}
	want := strings.ToLower(strings.TrimSuffix(path.Base(it.rel), path.Ext(it.rel)) + ".lrc")
	for _, f := range im.siblings(ctx, it.driveParent) {
		if strings.ToLower(f.Name) == want {
			if b, err := im.readDrive(ctx, f, 256<<10); err == nil {
				text, _ := media.DecodeText(b)
				im.lib.SetLyrics(ctx, trackID, library.LyricsLRC, text)
			}
			return
		}
	}
}
