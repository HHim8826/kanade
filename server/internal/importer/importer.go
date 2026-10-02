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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/media"
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
	progress     map[int64][2]int64 // item ID -> bytes sent, total
	covers       map[string]int64   // directory -> cover ID, for the current run
	albumArtists map[string]string  // batch|album folder|album -> decided album artist

	// OnBatchDone runs once when a batch has no pending items left; failed counts items
	// that need a retry. Client uploads use it to clear their staging folder.
	OnBatchDone func(ctx context.Context, kind, source string, failed int)
}

func New(d *sql.DB, lib *library.Store, drive Drive, stagingDir string, log *slog.Logger) *Importer {
	return &Importer{db: d, lib: lib, drive: drive, staging: stagingDir, log: log,
		wake: make(chan struct{}, 1), progress: map[int64][2]int64{}, covers: map[string]int64{}, albumArtists: map[string]string{}}
}

var audioExt = map[string]bool{
	".flac": true, ".mp3": true, ".m4a": true, ".mp4": true, ".aac": true, ".ogg": true, ".oga": true, ".opus": true,
	".wav": true, ".aif": true, ".aiff": true, ".ape": true, ".tak": true, ".wv": true, ".tta": true,
	".dsf": true, ".dff": true, ".wma": true,
}

// CreateBatch queues every audio file under dir (recursively) and wakes the worker.
func (im *Importer) CreateBatch(ctx context.Context, kind, source, dir string) (int64, int, error) {
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
	return im.CreateBatchFiles(ctx, kind, source, dir, files)
}

// CreateBatchFiles queues the audio files among paths (for example the files chosen in a
// torrent). root is the folder that relative paths and folder structure are taken from.
func (im *Importer) CreateBatchFiles(ctx context.Context, kind, source, root string, paths []string) (int64, int, error) {
	var files []string
	for _, p := range paths {
		if audioExt[strings.ToLower(filepath.Ext(p))] {
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				files = append(files, p)
			}
		}
	}
	if len(files) == 0 {
		return 0, 0, errors.New("no audio files found")
	}
	tx, err := im.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	now := db.Now()
	r, err := tx.ExecContext(ctx, `INSERT INTO import_batches (kind, source, state, created_at) VALUES (?, ?, 'running', ?)`, kind, source, now)
	if err != nil {
		return 0, 0, err
	}
	batchID, _ := r.LastInsertId()
	for _, p := range files {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			rel = filepath.Base(p)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO import_items (batch_id, local_path, rel_path, state, updated_at)
			VALUES (?, ?, ?, 'pending', ?)`, batchID, p, filepath.ToSlash(rel), now); err != nil {
			return 0, 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	im.Wake()
	return batchID, len(files), nil
}

func (im *Importer) Wake() {
	select {
	case im.wake <- struct{}{}:
	default:
	}
}

// Retry puts a batch's failed items back in the queue.
func (im *Importer) Retry(ctx context.Context, batchID int64) (int, error) {
	r, err := im.db.ExecContext(ctx, `UPDATE import_items SET state = 'pending', error = '', updated_at = ?
		WHERE batch_id = ? AND state = 'failed'`, db.Now(), batchID)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	if n > 0 {
		im.db.ExecContext(ctx, `UPDATE import_batches SET state = 'running', finished_at = NULL WHERE id = ?`, batchID)
		im.Wake()
	}
	return int(n), nil
}

// Run processes queued items one at a time until ctx ends (plan §6: one upload at a time).
func (im *Importer) Run(ctx context.Context) {
	// Items interrupted mid-upload resume from their saved session.
	im.db.ExecContext(ctx, `UPDATE import_items SET state = 'pending' WHERE state = 'uploading'`)
	for {
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
	id, batchID     int64
	path, rel, kind string
}

func (im *Importer) next(ctx context.Context) (*item, error) {
	var it item
	err := im.db.QueryRowContext(ctx, `SELECT i.id, i.batch_id, i.local_path, i.rel_path, b.kind FROM import_items i
		JOIN import_batches b ON b.id = i.batch_id WHERE i.state = 'pending' ORDER BY i.id LIMIT 1`).
		Scan(&it.id, &it.batchID, &it.path, &it.rel, &it.kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
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
	if _, err := im.db.ExecContext(ctx, `UPDATE import_items SET state = ?, error = ?, info = coalesce(?, info), sha256 = ?,
		asset_id = ?, track_id = ?, entry_id = ?, updated_at = ? WHERE id = ?`,
		out.state, out.msg, infoJSON, out.sha, nullable(out.assetID), nullable(out.trackID), nullable(out.entry), db.Now(), it.id); err != nil {
		im.log.Error("record import result", "item", it.id, "err", err)
	}
	im.mu.Lock()
	delete(im.progress, it.id)
	im.mu.Unlock()
	if it.kind == "upload" && out.state != StateFailed {
		im.removeStaged(it.path) // client uploads are copies; failed ones stay for a retry
	}
	res, err := im.db.ExecContext(ctx, `UPDATE import_batches SET state = 'done', finished_at = ? WHERE id = ? AND state != 'done'
		AND NOT EXISTS (SELECT 1 FROM import_items WHERE batch_id = ? AND state IN ('pending', 'uploading'))`,
		db.Now(), it.batchID, it.batchID)
	if n, _ := res.RowsAffected(); err == nil && n > 0 && im.OnBatchDone != nil {
		var source string
		var failed int
		im.db.QueryRowContext(ctx, `SELECT b.source, (SELECT count(*) FROM import_items WHERE batch_id = b.id AND state = 'failed')
			FROM import_batches b WHERE b.id = ?`, it.batchID).Scan(&source, &failed)
		im.OnBatchDone(ctx, it.kind, source, failed)
	}
}

func (im *Importer) setState(ctx context.Context, id int64, state string) {
	im.db.ExecContext(ctx, `UPDATE import_items SET state = ?, updated_at = ? WHERE id = ?`, state, db.Now(), id)
}

func (im *Importer) process(ctx context.Context, it *item) (outcome, error) {
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
		out.msg = fmt.Sprintf("%s (%s) is not playable yet; lossless conversion to FLAC comes in P2 (D2)", info.Format, info.Codec)
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
	in := entryInput(it.rel, info)
	if info.Tags.AlbumArtist == "" && in.Album != "" {
		in.AlbumArtist = im.albumArtistFor(ctx, it, in.Album)
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

	if in.Album != "" {
		in.CoverID = im.coverFor(ctx, it.path, info)
	}
	res, err := im.lib.Publish(ctx, asset.ID, in)
	if err != nil {
		return out, err
	}
	out.trackID, out.entry = res.TrackID, res.EntryID
	out.state = StatePublished
	if !res.Created {
		out.state = StateDuplicate
	}
	return out, nil
}

var (
	discDir     = regexp.MustCompile(`(?i)^(?:disc|disk|cd)\s*0*(\d{1,2})$`)
	leadingNum  = regexp.MustCompile(`^(\d{1,3})(?:[\s._\-]+|$)`)
	unsafeChars = strings.NewReplacer("/", "／", "\x00", "")
)

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
func (im *Importer) removeStaged(p string) {
	if err := os.Remove(p); err != nil {
		return
	}
	root := filepath.Clean(im.staging)
	for dir := filepath.Dir(p); strings.HasPrefix(dir, root+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil { // not empty
			return
		}
	}
}
