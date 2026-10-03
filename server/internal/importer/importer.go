// Package importer turns local audio files into library entries:
// probe → hash → dedupe → upload to Drive → verify → publish.
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
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/ffmpeg"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/media"
	"github.com/HHim8826/kanade/server/internal/staging"
)

// Item states.
const (
	StatePending   = "pending"
	StateUploading = "uploading"
	StatePublished = "published"
	StateDuplicate = "duplicate" // the library already had exactly this
	StateSkipped   = "skipped"   // not audio, or a format D2 does not play yet
	StateFailed    = "failed"
)

// Drive is the part of gdrive.Client the importer needs; tests substitute a fake.
type Drive interface {
	Folder(ctx context.Context, path string) (string, error)
	Upload(ctx context.Context, u gdrive.Upload) (gdrive.File, error)
}

type Importer struct {
	db      *sql.DB
	lib     *library.Store
	drive   Drive
	staging string
	log     *slog.Logger
	wake    chan struct{}

	mu           sync.Mutex
	progress     map[int64][2]int64       // item ID -> bytes sent, total
	covers       map[string]int64         // directory -> cover ID, for the current run
	driveDirs    map[string][]gdrive.File // inbox folder -> its files, for the current batch
	inboxWaiting atomic.Int64             // new inbox files the last scan left for later
	albumArtists map[string]string        // batch|album folder|album -> decided album artist

	// OnBatchDone runs when a batch has no pending items left, is canceled, or has its unsaved files
	// discarded; unsaved counts the items whose source must be kept (KeepsSource). Client uploads
	// use it to clear their staging folder.
	OnBatchDone func(ctx context.Context, kind, source string, unsaved int)
	// Budget is the staging budget shared with downloads and uploads (plan §6); data about to be
	// written to the work folder (unpacked archives, FFmpeg output, inbox fetches) is held on it
	// first. nil means no limit.
	Budget *staging.Budget
	// FFmpeg converts and splits (P2-4); nil when the server has none.
	FFmpeg *ffmpeg.Tool
	// SpaceWait is how long FFmpeg output waits for staging space that others hold before the file
	// fails (review #46); 0 fails at once.
	SpaceWait time.Duration
}

func New(d *sql.DB, lib *library.Store, drive Drive, stagingDir string, log *slog.Logger) *Importer {
	return &Importer{db: d, lib: lib, drive: drive, staging: stagingDir, log: log,
		wake: make(chan struct{}, 1), progress: map[int64][2]int64{}, covers: map[string]int64{}, albumArtists: map[string]string{},
		driveDirs: map[string][]gdrive.File{}}
}

var audioExt = map[string]bool{
	".flac": true, ".mp3": true, ".m4a": true, ".mp4": true, ".aac": true, ".ogg": true, ".oga": true, ".opus": true,
	".wav": true, ".aif": true, ".aiff": true, ".ape": true, ".tak": true, ".wv": true, ".tta": true,
	".dsf": true, ".dff": true, ".wma": true,
}

// CreateBatch queues every audio file, CUE or LOG sidecar and ZIP archive under dir (recursively)
// and wakes the worker. With preview the batch waits for the user after analysis.
func (im *Importer) CreateBatch(ctx context.Context, kind, source, dir string, preview bool) (int64, int, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return 0, 0, err
	}
	var files []string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return im.CreateBatchFiles(ctx, kind, source, dir, files, preview)
}

// ErrNothingToImport is a batch with no audio file or archive among its files.
var ErrNothingToImport = errors.New("no audio files found")

// CreateBatchFiles queues the audio files, sidecars and archives among paths (for example the files
// chosen in a torrent). root is the folder that relative paths and folder structure are taken from.
// The count returned is of audio files and archives.
func (im *Importer) CreateBatchFiles(ctx context.Context, kind, source, root string, paths []string, preview bool) (int64, int, error) {
	return im.CreateBatchLinked(ctx, kind, source, root, paths, preview, nil)
}

// CreateBatchLinked is CreateBatchFiles with link run in the same transaction, so that whoever
// hands the files over records the batch together with it, or neither happens (review #43).
func (im *Importer) CreateBatchLinked(ctx context.Context, kind, source, root string, paths []string, preview bool,
	link func(tx *sql.Tx, batchID int64) error) (int64, int, error) {
	type file struct{ path, role string }
	var files []file
	count := 0
	for _, p := range paths {
		ext := strings.ToLower(filepath.Ext(p))
		role := ""
		switch {
		case audioExt[ext]:
			role = RoleAudio
		case sidecarExt[ext]:
			role = RoleSidecar
		case ext == ".zip":
			role = RoleZip
		default:
			continue
		}
		if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() {
			continue
		}
		files = append(files, file{p, role})
		if role != RoleSidecar {
			count++
		}
	}
	if count == 0 {
		return 0, 0, ErrNothingToImport
	}
	tx, err := im.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	now := db.Now()
	r, err := tx.ExecContext(ctx, `INSERT INTO import_batches (kind, source, state, preview, created_at) VALUES (?, ?, ?, ?, ?)`,
		kind, source, BatchAnalyzing, preview, now)
	if err != nil {
		return 0, 0, err
	}
	batchID, _ := r.LastInsertId()
	for _, f := range files {
		rel, err := filepath.Rel(root, f.path)
		if err != nil {
			rel = filepath.Base(f.path)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO import_items (batch_id, local_path, rel_path, state, role, updated_at)
			VALUES (?, ?, ?, 'pending', ?, ?)`, batchID, f.path, filepath.ToSlash(rel), f.role, now); err != nil {
			return 0, 0, err
		}
	}
	if link != nil {
		if err := link(tx, batchID); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	im.Wake()
	return batchID, count, nil
}

func (im *Importer) Wake() {
	select {
	case im.wake <- struct{}{}:
	default:
	}
}

// Retry puts a batch's failed items back in the queue (review #18). The batch goes through
// analysis again, so a file that failed there is fetched, unpacked, converted or cut as it should
// be (a CUE sheet that was fixed is read again); files that already have a plan keep it, preview
// edits included, and new songs get groups of their own. Sidecars that found no album are tried
// again with the rest. An upload that failed resumes where it stopped.
func (im *Importer) Retry(ctx context.Context, batchID int64) (int, error) {
	tx, err := im.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := db.Now()
	r, err := tx.ExecContext(ctx, `UPDATE import_items SET state = 'pending', error = '', updated_at = ?
		WHERE batch_id = ? AND state = 'failed' AND batch_id IN (SELECT id FROM import_batches WHERE state = ?)`,
		now, batchID, BatchDone)
	if err != nil {
		return 0, err
	}
	n, err := r.RowsAffected()
	if err != nil || n == 0 {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE import_items SET state = 'pending', error = '', updated_at = ?
		WHERE batch_id = ? AND role = ? AND state = ?`, now, batchID, RoleSidecar, StateSkipped); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE import_batches SET state = ?, finished_at = NULL,
		options = json_set(coalesce(nullif(options, ''), '{}'), '$.rerun', json('true')) WHERE id = ?`, BatchAnalyzing, batchID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	im.Wake()
	return int(n), nil
}

// Run analyzes new batches and processes the items of running ones, one at a time, until ctx ends
// (plan §6: one upload at a time).
func (im *Importer) Run(ctx context.Context) {
	// Items interrupted mid-upload resume from their saved session.
	im.db.ExecContext(ctx, `UPDATE import_items SET state = 'pending' WHERE state = 'uploading'`)
	for {
		if id := im.nextAnalysis(ctx); id != 0 {
			if err := im.analyze(ctx, id); err != nil {
				if ctx.Err() != nil {
					return
				}
				im.log.Error("import analysis", "batch", id, "err", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(30 * time.Second):
				}
			}
			continue
		}
		it, err := im.next(ctx)
		if err != nil && ctx.Err() == nil {
			im.log.Error("import queue", "err", err)
		}
		if it == nil {
			select {
			case <-ctx.Done():
				return
			case <-im.wake:
			case <-time.After(30 * time.Second):
			}
			continue
		}
		im.handle(ctx, it)
	}
}

type item struct {
	id, batchID           int64
	path, rel, kind, role string
	plan                  *Plan // nil for items queued before P2-3
	temp                  bool
	// Made by FFmpeg from another file (P2-4).
	sourcePath, sourceKind, sourceSHA string
	sourceSize                        int64
	sourcePiece                       int    // the CUE track number of a cut song
	sourceCut                         string // and the sample range it was cut from
	// In the Drive inbox (P2-6, D6); sha is Drive's own checksum.
	driveID, driveParent, sha string
	driveSize                 int64
}

// next is the oldest waiting item of a running batch; a batch's sidecars wait for its audio, so
// they can join the album it made.
func (im *Importer) next(ctx context.Context) (*item, error) {
	var it item
	var plan string
	err := im.db.QueryRowContext(ctx, `SELECT i.id, i.batch_id, i.local_path, i.rel_path, b.kind, i.role, coalesce(i.plan, ''), i.temp,
		i.source_path, i.source_kind, i.source_sha256, i.source_size, i.source_piece, i.source_cut, i.drive_id, i.drive_parent, i.sha256, i.drive_size
		FROM import_items i JOIN import_batches b ON b.id = i.batch_id
		WHERE i.state = 'pending' AND b.state = ? AND i.role IN (?, ?) ORDER BY i.batch_id, i.role = ?, i.id LIMIT 1`,
		BatchRunning, RoleAudio, RoleSidecar, RoleSidecar).
		Scan(&it.id, &it.batchID, &it.path, &it.rel, &it.kind, &it.role, &plan, &it.temp,
			&it.sourcePath, &it.sourceKind, &it.sourceSHA, &it.sourceSize, &it.sourcePiece, &it.sourceCut, &it.driveID, &it.driveParent, &it.sha, &it.driveSize)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if plan != "" {
		it.plan = &Plan{}
		if err := json.Unmarshal([]byte(plan), it.plan); err != nil {
			it.plan = nil
		}
	}
	return &it, nil
}

type outcome struct {
	state, msg              string
	info                    *media.Info
	sha                     string
	assetID, trackID, entry int64
}

func (im *Importer) handle(ctx context.Context, it *item) {
	out, err := im.process(ctx, it)
	if ctx.Err() != nil {
		return // shutting down: the item stays resumable
	}
	if err != nil {
		out.state, out.msg = StateFailed, err.Error()
		im.log.Warn("import failed", "item", it.id, "path", it.rel, "err", err)
	}
	var infoJSON any
	if out.info != nil {
		b, _ := json.Marshal(out.info)
		infoJSON = string(b)
	}
	nullable := func(v int64) any {
		if v == 0 {
			return nil
		}
		return v
	}
	// Nothing is cleaned up until the result is recorded (review #2): if it cannot be, the source
	// stays and the item is done again after a restart (the library side is idempotent).
	var err2 error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 && !sleepCtx(ctx, time.Duration(attempt)*time.Second) {
			return
		}
		if _, err2 = im.db.ExecContext(ctx, `UPDATE import_items SET state = ?, error = ?, info = coalesce(?, info), sha256 = ?,
			asset_id = ?, track_id = ?, entry_id = ?, updated_at = ? WHERE id = ?`,
			out.state, out.msg, infoJSON, out.sha, nullable(out.assetID), nullable(out.trackID), nullable(out.entry), db.Now(), it.id); err2 == nil {
			break
		}
	}
	im.mu.Lock()
	delete(im.progress, it.id)
	im.mu.Unlock()
	if err2 != nil {
		im.log.Error("record import result; the source is kept", "item", it.id, "err", err2)
		return
	}
	if saved(out.state) { // only what is safely in the library; everything else stays for a retry or a decision (review #1)
		switch {
		case it.temp:
			im.removeUnder(im.workDir(it.batchID), it.path) // extracted from an archive, or made by FFmpeg
		case it.kind == "upload":
			im.removeStaged(it.path) // client uploads are copies
		}
	}
	im.finishIfIdle(ctx, it.batchID)
}

// saved is an item state whose file is in the library, verified in Drive.
func saved(state string) bool { return state == StatePublished || state == StateDuplicate }

// hold reserves need bytes of staging for data about to be written to the work folder; release
// once it is there (where WorkCommitted counts it) or gone.
func (im *Importer) hold(ctx context.Context, need int64) (release func(), err error) {
	if im.Budget == nil {
		return func() {}, nil
	}
	return im.Budget.Hold(ctx, need)
}

// holdOutput holds staging for FFmpeg output made from src, of size bytes. Output larger than
// the whole budget may be made alone, when staging holds little besides that source and the disk
// has room. When others hold the space, it waits for them up to SpaceWait, saying so on the item,
// rather than failing at once (review #46).
func (im *Importer) holdOutput(ctx context.Context, itemID, need int64, src string, size int64) (release func(), err error) {
	if im.Budget == nil {
		return func() {}, nil
	}
	var own int64 // the source counts as the request's own only where the budget counts it
	if dir := im.Budget.Dir; dir != "" {
		if rel, err := filepath.Rel(dir, src); err == nil && filepath.IsLocal(rel) {
			own = size
		}
	}
	deadline := time.Now().Add(im.SpaceWait)
	waiting := false
	defer func() {
		if waiting {
			im.db.ExecContext(context.WithoutCancel(ctx), `UPDATE import_items SET error = '' WHERE id = ? AND error LIKE 'waiting for staging space%'`, itemID)
		}
	}()
	for {
		release, err = im.Budget.HoldRequest(ctx, staging.Request{Need: need, Alone: true, Own: own})
		if err == nil || !(errors.Is(err, staging.ErrOverBudget) || errors.Is(err, staging.ErrReserve)) || !time.Now().Before(deadline) {
			return release, err
		}
		if !waiting {
			waiting = true
			im.db.ExecContext(ctx, `UPDATE import_items SET error = ?, updated_at = ? WHERE id = ?`,
				"waiting for staging space: "+err.Error(), db.Now(), itemID)
		}
		if !sleepCtx(ctx, 5*time.Second) {
			return nil, ctx.Err()
		}
	}
}

// StateDiscarded is an unsaved file the user chose to let go of, so its source can be cleaned up.
const StateDiscarded = "discarded"

// KeepsSource is the SQL condition (on import_items aliased as alias) for items whose source must
// be kept: not finished, failed, or audio (and archives) that never made it into the library, until
// the user discards them (review #1). Files the user excluded in the preview do not count.
func KeepsSource(alias string) string {
	return fmt.Sprintf(`(%[1]s.state IN ('pending', 'uploading', 'failed') OR (%[1]s.role IN ('audio', 'zip') AND %[1]s.state = 'skipped'))`, alias)
}

// Unsaved counts a batch's items whose source must be kept.
func (im *Importer) Unsaved(ctx context.Context, batchID int64) (int, error) {
	var n int
	err := im.db.QueryRowContext(ctx, `SELECT count(*) FROM import_items i WHERE i.batch_id = ? AND `+KeepsSource("i"), batchID).Scan(&n)
	return n, err
}

// Discard lets go of a finished batch's unsaved files (failed, or skipped audio): they are marked
// discarded and the batch's sources are cleaned up as if they had been imported.
func (im *Importer) Discard(ctx context.Context, batchID int64) (int, error) {
	r, err := im.db.ExecContext(ctx, `UPDATE import_items SET state = ?, updated_at = ? WHERE batch_id = ?
		AND state IN ('failed', 'skipped') AND role IN ('audio', 'zip')
		AND batch_id IN (SELECT id FROM import_batches WHERE state = ?)`, StateDiscarded, db.Now(), batchID, BatchDone)
	if err != nil {
		return 0, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return 0, err
	}
	// Failed sidecars go with them: there is nothing left to attach them to.
	if _, err := im.db.ExecContext(ctx, `UPDATE import_items SET state = ?, updated_at = ? WHERE batch_id = ? AND state = 'failed'`,
		StateDiscarded, db.Now(), batchID); err != nil {
		return int(n), err
	}
	if n > 0 {
		im.batchDone(ctx, batchID)
	}
	return int(n), nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// batchDone runs once a batch has nothing left to do: its work folder (unpacked archives, FFmpeg
// output) is deleted when every file is saved or let go of, and OnBatchDone lets client uploads
// clear their staging folder.
func (im *Importer) batchDone(ctx context.Context, batchID int64) {
	var kind, source string
	if err := im.db.QueryRowContext(ctx, `SELECT kind, source FROM import_batches WHERE id = ?`, batchID).Scan(&kind, &source); err != nil {
		im.log.Warn("finished batch", "batch", batchID, "err", err)
		return
	}
	unsaved, err := im.Unsaved(ctx, batchID)
	if err != nil {
		im.log.Warn("finished batch", "batch", batchID, "err", err)
		return
	}
	// The work folder goes unless an unsaved file lives there (unpacked from an archive, made by
	// FFmpeg, or fetched from the inbox) or was made from one that does.
	work := im.workDir(batchID)
	var inWork int
	if err := im.db.QueryRowContext(ctx, `SELECT count(*) FROM import_items i WHERE i.batch_id = ?1 AND `+KeepsSource("i")+`
		AND (substr(i.local_path, 1, length(?2)) = ?2 OR substr(i.source_path, 1, length(?2)) = ?2)`,
		batchID, work+string(filepath.Separator)).Scan(&inWork); err != nil {
		im.log.Warn("finished batch", "batch", batchID, "err", err)
	} else if inWork == 0 {
		os.RemoveAll(work)
	}
	im.mu.Lock()
	clear(im.driveDirs) // the next inbox batch lists folders afresh
	im.mu.Unlock()
	if kind == "inbox" {
		if err := im.tidyInbox(ctx, batchID); err != nil {
			im.log.Warn("tidy inbox", "batch", batchID, "err", err)
		}
	}
	if im.OnBatchDone != nil {
		im.OnBatchDone(ctx, kind, source, unsaved)
	}
}

func (im *Importer) setState(ctx context.Context, id int64, state string) {
	im.db.ExecContext(ctx, `UPDATE import_items SET state = ?, updated_at = ? WHERE id = ?`, state, db.Now(), id)
}

func (im *Importer) process(ctx context.Context, it *item) (outcome, error) {
	if it.role == RoleSidecar {
		return im.processSidecar(ctx, it)
	}
	if it.driveID != "" && it.path == "" {
		return im.processDrive(ctx, it) // in the Drive inbox: imported where it is
	}
	var out outcome
	f, err := os.Open(it.path)
	if err != nil {
		return out, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return out, err
	}
	info, err := media.Probe(f, st.Size())
	if errors.Is(err, media.ErrUnknownFormat) {
		out.state, out.msg = StateSkipped, "not a recognized audio file"
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("cannot read audio: %w", err)
	}
	out.info = info
	if !info.Playable {
		out.state = StateSkipped
		out.msg = unplayable(info) // a convertible file reaches here only from a batch queued before P2-4
		return out, nil
	}

	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, st.Size())); err != nil {
		return out, err
	}
	out.sha = hex.EncodeToString(h.Sum(nil))

	asset, err := im.lib.AssetByHash(ctx, out.sha, st.Size())
	if err != nil {
		return out, err
	}
	if asset == nil {
		if asset, err = im.lib.CreateAsset(ctx, library.Asset{SHA256: out.sha, Size: st.Size(), Format: info.Format, Codec: info.Codec,
			SampleRate: info.SampleRate, BitDepth: info.BitDepth, Channels: info.Channels, DurationMS: info.DurationMS,
			Bitrate: info.Bitrate, AudioMD5: info.AudioMD5}); err != nil {
			return out, err
		}
	}
	out.assetID = asset.ID
	var in library.EntryInput
	if it.plan != nil {
		in = it.plan.input()
		in.AlbumID = im.groupAlbum(ctx, it.batchID, it.plan.Group)
	} else { // queued before plans existed
		in = entryInput(it.rel, info)
		if info.Tags.AlbumArtist == "" && in.Album != "" {
			in.AlbumArtist = im.albumArtistFor(ctx, it, in.Album)
		}
	}

	if asset.State != library.AssetVerified {
		im.setState(ctx, it.id, StateUploading)
		folder, err := im.drive.Folder(ctx, drivePath(in))
		if err != nil {
			return out, err
		}
		file, err := im.drive.Upload(ctx, gdrive.Upload{
			Path: it.path, Name: filepath.Base(it.path), ParentID: folder, MIME: mimeOf(info),
			Size: st.Size(), SHA256: out.sha, Sessions: &sessionStore{db: im.db, assetID: asset.ID},
			Progress: func(sent, total int64) {
				im.mu.Lock()
				im.progress[it.id] = [2]int64{sent, total}
				im.mu.Unlock()
			},
		})
		if err != nil {
			return out, fmt.Errorf("upload: %w", err)
		}
		if err := im.lib.MarkVerified(ctx, asset.ID, file.ID); err != nil {
			return out, err
		}
	}

	// Covers and lyrics lie next to the original, not next to a converted or cut copy.
	beside := it.path
	if it.sourcePath != "" {
		beside = it.sourcePath
	}
	switch {
	case in.Album == "":
	case it.driveParent != "": // fetched from the Drive inbox: its folder is there
		in.CoverID = im.driveCover(ctx, it, info)
	default:
		in.CoverID = im.coverFor(ctx, beside, info)
	}
	res, err := im.lib.Publish(ctx, asset.ID, in)
	if err != nil {
		return out, err
	}
	out.trackID, out.entry = res.TrackID, res.EntryID
	im.sourceOf(ctx, it, asset.ID)
	switch {
	case it.driveParent != "" && it.sourceKind != SourceSplit:
		im.driveLyrics(ctx, it, res.TrackID, info)
	case it.sourceKind == SourceSplit:
		im.importLyrics(ctx, it.path, res.TrackID, info) // a song cut from an image has no .lrc of its own
	default:
		im.importLyrics(ctx, beside, res.TrackID, info)
	}
	out.state = StatePublished
	if !res.Created {
		out.state = StateDuplicate
	}
	return out, nil
}

// importLyrics keeps the file's own lyrics, and a same-named .lrc next to it (decision D2 §5).
// The .lrc goes second so it wins, unless it is plain text and the embedded lyrics are timed.
func (im *Importer) importLyrics(ctx context.Context, path string, trackID int64, info *media.Info) {
	if info.Tags.Lyrics != "" {
		im.lib.SetLyrics(ctx, trackID, library.LyricsEmbedded, info.Tags.Lyrics)
	}
	base := strings.TrimSuffix(path, filepath.Ext(path))
	for _, ext := range []string{".lrc", ".LRC", ".Lrc"} {
		st, err := os.Stat(base + ext)
		if err != nil || st.Size() > 256<<10 {
			continue
		}
		if b, err := os.ReadFile(base + ext); err == nil {
			text, _ := media.DecodeText(b)
			if _, err := im.lib.SetLyrics(ctx, trackID, library.LyricsLRC, text); err != nil {
				im.log.Warn("store lyrics", "track", trackID, "err", err)
			}
		}
		return
	}
}

var (
	discDir     = regexp.MustCompile(`(?i)^(?:disc|disk|cd)\s*0*(\d{1,2})$`)
	leadingNum  = regexp.MustCompile(`^(\d{1,3})(?:[\s._\-]+|$)`)
	trailingNum = regexp.MustCompile(`^[A-Za-z]{1,12}[\s._\-]*0*(\d{1,3})$`) // "DUE01", "tri14", "Track 05"
	unsafeChars = strings.NewReplacer("/", "／", "\x00", "")
)

// Folder names as album names: downloads put the release date in front and format notes behind
// ("[2024.09.30] Album [FLAC 96kHz／24bit]"); some folders name nothing in particular.
var (
	folderDate   = regexp.MustCompile(`^\s*[\[(（【]\s*\d{2,4}[.\-/年]\d{1,2}(?:[.\-/月]\d{1,2}日?)?\s*[\])）】]\s*`)
	folderFormat = regexp.MustCompile(`(?i)\s*[\[(（【][^\[\]()（）【】]*(?:flac|mp3|aac|m4a|alac|wav|ape|ogg|opus|kbps|\d\s*k\b|khz|\d\s*bit|hi-?res|\bweb\b|vbr|cbr|cd-?rip)[^\[\]()（）【】]*[\])）】]\s*$`)
	folderVague  = map[string]bool{"music": true, "musics": true, "mp3": true, "flac": true, "audio": true, "songs": true,
		"download": true, "downloads": true, "new folder": true, "untitled folder": true, "新しいフォルダー": true,
		"新しいフォルダ": true, "新建文件夹": true, "音楽": true, "音乐": true, "歌曲": true}
)

// FolderAlbum is the album an album folder (relative to the batch, Disc 1/ already taken off) names
// for files whose tags name none (plan §4: tags, then folders, then file names): its last part
// without the release date and format notes. The top of the batch and vague names give "".
func FolderAlbum(root string) string {
	if root == "." || root == "" || root == "/" {
		return ""
	}
	name := folderDate.ReplaceAllString(path.Base(filepath.ToSlash(root)), "")
	for {
		trimmed := folderFormat.ReplaceAllString(name, "")
		if trimmed == name {
			break
		}
		name = trimmed
	}
	name = strings.TrimSpace(name)
	if folderVague[strings.ToLower(name)] {
		return ""
	}
	return name
}

// entryInput applies the evidence order of plan §4: tags first, then the folder structure,
// then the file name. Nothing is invented: no album tag means a standalone track.
func entryInput(rel string, info *media.Info) library.EntryInput {
	t := info.Tags
	base := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	in := library.EntryInput{
		Title: t.Title, Artist: t.Artist, Album: t.Album, AlbumArtist: t.AlbumArtist,
		Date: t.Date, DiscNo: t.DiscNo, TrackNo: t.TrackNo,
	}
	if in.Title == "" {
		in.Title = strings.TrimSpace(leadingNum.ReplaceAllString(base, ""))
		if in.Title == "" {
			in.Title = base
		}
	}
	if in.TrackNo == 0 {
		if m := leadingNum.FindStringSubmatch(base); m != nil {
			in.TrackNo, _ = strconv.Atoi(m[1])
		} else if m := trailingNum.FindStringSubmatch(base); m != nil {
			in.TrackNo, _ = strconv.Atoi(m[1])
		}
	}
	if in.DiscNo == 0 {
		if m := discDir.FindStringSubmatch(filepath.Base(filepath.Dir(rel))); m != nil {
			in.DiscNo, _ = strconv.Atoi(m[1])
		}
	}
	if in.Artist == "" {
		in.Artist = in.AlbumArtist
	}
	if in.AlbumArtist == "" {
		in.AlbumArtist = in.Artist
	}
	if spokenGenre.MatchString(t.Genre) || spokenPath.MatchString(rel) || spokenPath.MatchString(t.Album) {
		in.Kind = "spoken"
	}
	return in
}

// Drama CDs and radio are 44 % of the surveyed library; they resume where they stopped and stay
// out of random music picks. Same rules as the backfill in migration 0005.
var (
	spokenGenre = regexp.MustCompile(`(?i)^(spoken( word)?|drama|radio|audio ?drama|audiobook)$|朗読|ドラマ`)
	spokenPath  = regexp.MustCompile(`(?i)drama ?cd|\bradio\b|djcd|ドラマ|ラジオ`) // whole word: not Radiohead
)

// drivePath is where a file lands in Drive: library/<album artist>/<album>. It is only a
// convenience for browsing Drive; the database stays the source of truth.
func drivePath(in library.EntryInput) string {
	clean := func(s, fallback string) string {
		s = strings.TrimSpace(unsafeChars.Replace(s))
		if r := []rune(s); len(r) > 120 {
			s = string(r[:120])
		}
		if s == "" || s == "." || s == ".." {
			return fallback
		}
		return s
	}
	return "library/" + clean(in.AlbumArtist, "Unknown Artist") + "/" + clean(in.Album, "Singles")
}

func mimeOf(info *media.Info) string {
	switch info.Format {
	case "flac":
		return "audio/flac"
	case "mp3":
		return "audio/mpeg"
	case "m4a":
		return "audio/mp4"
	case "ogg", "opus":
		return "audio/ogg"
	}
	return "application/octet-stream"
}

// sessionStore keeps an asset's resumable-upload session in drive_uploads.
type sessionStore struct {
	db      *sql.DB
	assetID int64
}

func (s *sessionStore) LoadSession(ctx context.Context) (string, error) {
	var uri string
	err := s.db.QueryRowContext(ctx, `SELECT session_uri FROM drive_uploads WHERE asset_id = ?`, s.assetID).Scan(&uri)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return uri, err
}

func (s *sessionStore) SaveSession(ctx context.Context, uri string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO drive_uploads (asset_id, session_uri, created_at) VALUES (?, ?, ?)
		ON CONFLICT (asset_id) DO UPDATE SET session_uri = excluded.session_uri, created_at = excluded.created_at`,
		s.assetID, uri, db.Now())
	return err
}

func (s *sessionStore) DeleteSession(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM drive_uploads WHERE asset_id = ?`, s.assetID)
	return err
}

// albumRoot treats Disc 1/, CD2/ ... as part of the album folder above them.
func albumRoot(dir string) string {
	if discDir.MatchString(filepath.Base(dir)) {
		return filepath.Dir(dir)
	}
	return dir
}

// albumArtistFor decides the album artist for a file that has none, from the other files of the
// same album in the same album folder: an explicit album artist on any of them wins; one shared
// performer is used as is; several performers make it "Various Artists". Without this an OST, or a
// single whose instrumentals credit the composer, splits into one album per performer.
func (im *Importer) albumArtistFor(ctx context.Context, it *item, album string) string {
	root := albumRoot(filepath.Dir(it.path))
	key := fmt.Sprintf("%d|%s|%s", it.batchID, root, album)
	im.mu.Lock()
	v, ok := im.albumArtists[key]
	im.mu.Unlock()
	if ok {
		return v
	}
	rows, err := im.db.QueryContext(ctx, `SELECT local_path FROM import_items WHERE batch_id = ?`, it.batchID)
	if err != nil {
		return ""
	}
	var paths []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil && albumRoot(filepath.Dir(p)) == root {
			paths = append(paths, p)
		}
	}
	rows.Close()

	explicit := map[string]int{}
	artists := map[string]bool{}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		st, err := f.Stat()
		var info *media.Info
		if err == nil {
			info, err = media.Probe(f, st.Size())
		}
		f.Close()
		if err != nil || info.Tags.Album != album {
			continue
		}
		if a := info.Tags.AlbumArtist; a != "" {
			explicit[a]++
		} else if a := info.Tags.Artist; a != "" {
			artists[a] = true
		}
	}
	decided := ""
	switch {
	case len(explicit) > 0:
		best := 0
		for a, n := range explicit {
			if n > best || (n == best && a < decided) {
				decided, best = a, n
			}
		}
	case len(artists) == 1:
		for a := range artists {
			decided = a
		}
	case len(artists) > 1:
		decided = "Various Artists"
	}
	im.mu.Lock()
	im.albumArtists[key] = decided
	im.mu.Unlock()
	return decided
}

// removeStaged deletes an imported upload and any folders it leaves empty, up to the staging root.
func (im *Importer) removeStaged(p string) { im.removeUnder(im.staging, p) }

// removeUnder deletes a file and the folders it leaves empty, up to root.
func (im *Importer) removeUnder(root, p string) {
	if err := os.Remove(p); err != nil {
		return
	}
	root = filepath.Clean(root)
	for dir := filepath.Dir(p); strings.HasPrefix(dir, root+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil { // not empty
			return
		}
	}
}

// Original reads a track back from its first import record: the saved tags, interpreted the way
// that import did (P2-2 "restore original tags"). It returns nil when the track has no record.
// The album artist is whatever the tags said; an album artist decided from sibling files is kept
// in the album's origin instead.
func (im *Importer) Original(ctx context.Context, trackID int64) (*library.EntryInput, error) {
	var rel, raw, opts string
	err := im.db.QueryRowContext(ctx, `SELECT i.rel_path, i.info, b.options FROM import_items i JOIN import_batches b ON b.id = i.batch_id
		WHERE i.track_id = ? AND i.info IS NOT NULL ORDER BY i.id LIMIT 1`, trackID).Scan(&rel, &raw, &opts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var info media.Info
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return nil, fmt.Errorf("import record of track %d: %w", trackID, err)
	}
	var o batchOptions
	if json.Unmarshal([]byte(opts), &o) == nil && o.Encoding != "" {
		info.Redecode(o.Encoding) // the encoding the preview chose is part of how the tags were read
	}
	in := entryInput(rel, &info)
	return &in, nil
}

// StoreCover keeps an image (JPEG or PNG) as a cover, uploading it once per content.
func (im *Importer) StoreCover(ctx context.Context, data []byte) (int64, error) {
	if len(data) > maxCoverFile {
		return 0, errors.New("the image is larger than 16 MB")
	}
	if id := im.storeCover(ctx, data); id != 0 {
		return id, nil
	}
	return 0, errors.New("not a JPEG or PNG image, or the upload to Drive failed")
}
