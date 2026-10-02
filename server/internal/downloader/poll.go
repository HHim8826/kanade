package downloader

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Run advances downloads every two seconds, or sooner when an API call pokes it.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
		if s.aria.Ready() {
			s.tick(ctx)
		}
	}
}

func (s *Service) tick(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT `+rowCols+` FROM downloads
		WHERE state NOT IN (?, ?, ?) OR files_removed = 0 ORDER BY id`, StateCompleted, StateFailed, StateCanceled)
	if err != nil {
		s.log.Error("downloads", "err", err)
		return
	}
	var list []*row
	for rows.Next() {
		if r, err := scanRow(rows); err == nil {
			list = append(list, r)
		}
	}
	rows.Close()

	active := false
	for _, r := range list {
		switch r.State {
		case StateMetadata:
			s.pollMetadata(ctx, r)
		case StateDownloading:
			s.pollTransfer(ctx, r)
			active = active || r.State == StateDownloading
		case StateSeeding:
			s.pollTransfer(ctx, r)
		case StateCompleted, StateCanceled, StateFailed:
			s.cleanup(ctx, r)
		}
		if r.State == StateDownloading {
			active = true
		}
	}
	if !active { // one download at a time (plan §6); start the oldest queued one
		for _, r := range list {
			if r.State != StateQueued {
				continue
			}
			if err := s.aria.RPC.Call(ctx, "unpause", nil, r.gid); err != nil {
				s.setState(ctx, r.ID, StateFailed, err.Error())
				continue
			}
			s.setState(ctx, r.ID, StateDownloading, "")
			break
		}
	}
}

func (s *Service) fail(ctx context.Context, r *row, msg string) {
	r.State = StateFailed
	s.setState(ctx, r.ID, StateFailed, msg)
}

func (s *Service) pollMetadata(ctx context.Context, r *row) {
	if r.gid == "" { // a magnet still fetching its metadata
		var st Status
		err := s.aria.RPC.Call(ctx, "tellStatus", &st, r.metaGID, []string{"status", "infoHash", "errorMessage"})
		switch {
		case IsNotFound(err):
			s.fail(ctx, r, "aria2 lost the task")
			return
		case err != nil:
			return
		}
		switch st.Status {
		case "complete":
			// bt-save-metadata wrote <info hash>.torrent into the download folder.
			torrent, err := os.ReadFile(filepath.Join(r.dir, strings.ToLower(st.InfoHash)+".torrent"))
			if err != nil {
				s.fail(ctx, r, "metadata arrived but the torrent file is missing: "+err.Error())
				return
			}
			gid, err := s.addTorrent(ctx, torrent, r.dir)
			if err != nil {
				s.fail(ctx, r, err.Error())
				return
			}
			s.aria.RPC.Call(ctx, "removeDownloadResult", nil, r.metaGID)
			r.gid = gid
			s.db.ExecContext(ctx, `UPDATE downloads SET gid = ?, updated_at = ? WHERE id = ?`, r.gid, db.Now(), r.ID)
		case "error":
			s.fail(ctx, r, "fetching the metadata failed: "+st.ErrorMessage)
			return
		case "removed":
			s.setState(ctx, r.ID, StateCanceled, "")
			return
		default:
			return // still looking for peers that have the metadata
		}
	}

	var files []File
	if err := s.aria.RPC.Call(ctx, "getFiles", &files, r.gid); err != nil {
		if IsNotFound(err) {
			s.fail(ctx, r, "aria2 lost the task")
		}
		return
	}
	var st Status
	if err := s.aria.RPC.Call(ctx, "tellStatus", &st, r.gid, []string{"infoHash", "bittorrent"}); err != nil {
		return
	}
	var other int64
	if st.InfoHash != "" && s.db.QueryRowContext(ctx, `SELECT id FROM downloads WHERE info_hash = ? AND id != ?
		AND state NOT IN (?, ?, ?)`, st.InfoHash, r.ID, StateCompleted, StateFailed, StateCanceled).Scan(&other) == nil {
		s.aria.RPC.Call(ctx, "forceRemove", nil, r.gid)
		s.fail(ctx, r, "this torrent is already download #"+strconv.FormatInt(other, 10))
		return
	}
	views := make([]FileView, 0, len(files))
	for _, f := range files {
		idx, _ := strconv.Atoi(f.Index)
		rel, err := filepath.Rel(r.dir, f.Path)
		if err != nil {
			rel = f.Path
		}
		views = append(views, FileView{Index: idx, Path: filepath.ToSlash(rel), Length: num(f.Length)})
	}
	suggest(views)
	name := st.Bittorrent.Info.Name
	if name == "" && len(views) > 0 {
		name = strings.SplitN(views[0].Path, "/", 2)[0]
	}
	data, _ := json.Marshal(views)
	r.State = StateSelecting
	s.db.ExecContext(ctx, `UPDATE downloads SET name = ?, info_hash = ?, files = ?, state = ?, updated_at = ? WHERE id = ?`,
		name, st.InfoHash, string(data), StateSelecting, db.Now(), r.ID)
}

func (s *Service) pollTransfer(ctx context.Context, r *row) {
	var st Status
	err := s.aria.RPC.Call(ctx, "tellStatus", &st, r.gid, []string{"status", "totalLength", "completedLength",
		"uploadLength", "downloadSpeed", "uploadSpeed", "connections", "seeder", "errorMessage"})
	if IsNotFound(err) {
		if r.State == StateSeeding {
			r.State = StateCompleted
			s.setState(ctx, r.ID, StateCompleted, "")
		} else {
			s.fail(ctx, r, "aria2 lost the task")
		}
		return
	}
	if err != nil {
		return
	}
	// aria2 counts whole pieces, which can spill into unselected neighbours; cap at 100 %.
	done := num(st.CompletedLength)
	if r.TotalBytes > 0 { // the selected files' size; aria2's own total is piece-aligned and can be larger
		done = min(done, r.TotalBytes)
	}
	s.db.ExecContext(ctx, `UPDATE downloads SET done_bytes = ?, uploaded_bytes = ?, down_speed = ?, up_speed = ?,
		peers = ?, updated_at = ? WHERE id = ?`, done, num(st.UploadLength), num(st.DownloadSpeed),
		num(st.UploadSpeed), num(st.Connections), db.Now(), r.ID)
	switch st.Status {
	case "error":
		s.fail(ctx, r, st.ErrorMessage)
		return
	case "removed":
		r.State = StateCanceled
		s.setState(ctx, r.ID, StateCanceled, "")
		return
	}
	finished := st.Status == "complete" || st.Seeder == "true" ||
		(num(st.TotalLength) > 0 && num(st.CompletedLength) >= num(st.TotalLength))
	switch {
	case r.State == StateDownloading && finished:
		s.startImport(ctx, r)
		next := StateSeeding
		if st.Status == "complete" {
			next = StateCompleted // nothing to seed (for example ratio already met)
		}
		r.State = next
		s.db.ExecContext(ctx, `UPDATE downloads SET state = ?, completed_at = ?, down_speed = 0, updated_at = ? WHERE id = ?`,
			next, db.Now(), db.Now(), r.ID)
	case r.State == StateSeeding && st.Status == "complete":
		r.State = StateCompleted
		s.setState(ctx, r.ID, StateCompleted, "")
	}
}

// startImport queues exactly the files the user selected.
func (s *Service) startImport(ctx context.Context, r *row) {
	var paths []string
	for _, f := range r.files {
		if f.Selected {
			paths = append(paths, filepath.Join(r.dir, filepath.FromSlash(f.Path)))
		}
	}
	batch, n, err := s.imp.CreateBatchFiles(ctx, "download", r.Name, r.dir, paths)
	if err != nil {
		s.db.ExecContext(ctx, `UPDATE downloads SET error = ? WHERE id = ?`, "import: "+err.Error(), r.ID)
		return
	}
	r.ImportBatchID = batch
	s.db.ExecContext(ctx, `UPDATE downloads SET import_batch_id = ? WHERE id = ?`, batch, r.ID)
	s.log.Info("download complete; import queued", "download", r.ID, "batch", batch, "files", n)
}

var (
	videoExt   = map[string]bool{".mkv": true, ".mp4": true, ".m2ts": true, ".ts": true, ".avi": true, ".vob": true, ".iso": true, ".wmv": true, ".mov": true, ".webm": true, ".flv": true}
	sidecarExt = map[string]bool{".cue": true, ".log": true, ".lrc": true, ".m3u": true, ".m3u8": true, ".accurip": true}
	imageExt   = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".bmp": true, ".tif": true, ".tiff": true, ".gif": true, ".webp": true}
	coverLike  = regexp.MustCompile(`(?i)(cover|front|folder|jacket)|[_-]0*1$`)
	audioExt   = map[string]bool{".flac": true, ".mp3": true, ".m4a": true, ".aac": true, ".ogg": true, ".oga": true, ".opus": true,
		".wav": true, ".aif": true, ".aiff": true, ".ape": true, ".tak": true, ".wv": true, ".tta": true, ".dsf": true, ".dff": true, ".wma": true}
)

const scansLimit = 300 << 20

// suggest marks the default selection from decision D2: audio, cue/log/lrc and small text files;
// images too unless they total over 300 MB, then only cover-like ones; never video.
func suggest(files []FileView) {
	var images int64
	for _, f := range files {
		if imageExt[strings.ToLower(filepath.Ext(f.Path))] {
			images += f.Length
		}
	}
	for i := range files {
		f := &files[i]
		ext := strings.ToLower(filepath.Ext(f.Path))
		base := strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
		switch {
		case audioExt[ext], sidecarExt[ext]:
			f.Suggested = true
		case videoExt[ext]:
			f.Suggested = false
		case imageExt[ext]:
			f.Suggested = images <= scansLimit || coverLike.MatchString(base)
		default:
			f.Suggested = f.Length <= 1<<20
		}
	}
}
