package downloader

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/staging"
)

// Run advances downloads every two seconds, or sooner when an API call pokes it.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	secured := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
		}
		if !secured { // downloads from before task.torrent keep their own before a round needs it
			s.secureTorrents(ctx)
			secured = true
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
		case StateImporting:
			s.afterRound(ctx, r)
		case StateCompleted, StateCanceled, StateFailed:
			s.cleanup(ctx, r)
		}
		if r.State == StateDownloading {
			active = true
		}
	}
	if !active && !s.lowDisk.Load() { // one download at a time (plan §6); start the oldest queued one that fits
		for _, r := range list {
			if r.State != StateQueued {
				continue
			}
			err := s.startRound(ctx, r)
			if err == nil {
				break
			}
			if errors.Is(err, staging.ErrOverBudget) || errors.Is(err, staging.ErrReserve) {
				s.setNote(ctx, r, notePrefixSpace+err.Error())
				continue // a smaller one further down may fit
			}
			s.setState(ctx, r.ID, StateFailed, err.Error())
		}
	}
}

const notePrefixSpace = "waiting for staging space: "

func (s *Service) setNote(ctx context.Context, r *row, note string) {
	if r.Note != note {
		r.Note = note
		s.db.ExecContext(ctx, `UPDATE downloads SET note = ?, updated_at = ? WHERE id = ?`, note, db.Now(), r.ID)
	}
}

// startRound starts a queued download: a round already under way (paused, say) continues; else the
// next round is planned within the staging space left and reserved before it starts (review #4,
// #28).
func (s *Service) startRound(ctx context.Context, r *row) error {
	if r.roundBytes > 0 && r.gid != "" {
		if err := s.aria.RPC.Call(ctx, "unpause", nil, r.gid); err != nil {
			return err
		}
		s.setState(ctx, r.ID, StateDownloading, "")
		return nil
	}
	avail := s.budget.Limit - s.budget.Used(ctx)
	pick, need, work, alone := planRound(r.files, max(avail, 0))
	if len(pick) == 0 {
		return errors.New("no file left to download")
	}
	return s.budget.Take(ctx, staging.Request{Need: need + work, MakeRoom: s.stopOldestSeed, Alone: alone, Record: func() error {
		return s.beginRound(ctx, r, pick, need, work)
	}})
}

// beginRound selects a round's files in aria2 and records the reservation: need bytes to download,
// and work for the import (review #46). After an earlier round the aria2 task is gone; it is added
// again from the download's torrent, fetching only these files.
func (s *Service) beginRound(ctx context.Context, r *row, pick []int, need, work int64) error {
	round := r.Round + 1
	list := make([]string, len(pick))
	in := map[int]bool{}
	for i, idx := range pick {
		list[i] = strconv.Itoa(idx)
		in[idx] = true
	}
	gid := r.gid
	if gid == "" {
		torrent, err := s.torrentFor(ctx, r)
		if err != nil {
			return fmt.Errorf("cannot start round %d: %w", round, err)
		}
		if gid, err = s.addTorrent(ctx, torrent, r.dir, true); err != nil {
			return err
		}
	}
	if err := s.aria.RPC.Call(ctx, "changeOption", nil, gid, map[string]string{"select-file": strings.Join(list, ",")}); err != nil {
		return err
	}
	if err := s.aria.RPC.Call(ctx, "unpause", nil, gid); err != nil {
		return err
	}
	for i := range r.files {
		if in[r.files[i].Index] {
			r.files[i].Round = round
		}
	}
	note := ""
	if left := len(remaining(r.files)); left > 0 || round > 1 {
		note = fmt.Sprintf("round %d: %d files, %d MB; %d files wait for later rounds", round, len(pick), need>>20, left)
	}
	files, _ := json.Marshal(r.files)
	r.gid, r.Round, r.roundBytes, r.roundWork, r.State = gid, round, need, work, StateDownloading
	_, err := s.db.ExecContext(ctx, `UPDATE downloads SET gid = ?, files = ?, round = ?, round_bytes = ?, round_work = ?, state = ?,
		note = ?, error = '', updated_at = ? WHERE id = ?`, gid, string(files), round, need, work, StateDownloading, note, db.Now(), r.ID)
	return err
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
			if err := saveTorrent(r.dir, torrent); err != nil {
				s.fail(ctx, r, err.Error())
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
	if r.AutoSelect { // an RSS rule started it: take the suggested files; on a problem, wait for the user
		var pick []int
		for _, v := range views {
			if v.Suggested {
				pick = append(pick, v.Index)
			}
		}
		err := errors.New("no file is suggested")
		if len(pick) > 0 {
			err = s.selectLocked(ctx, r.ID, pick)
		}
		if err != nil {
			s.db.ExecContext(ctx, `UPDATE downloads SET error = ?, updated_at = ? WHERE id = ?`,
				"automatic selection: "+err.Error()+"; choose the files yourself", db.Now(), r.ID)
		}
	}
}

func (s *Service) pollTransfer(ctx context.Context, r *row) {
	var st Status
	err := s.aria.RPC.Call(ctx, "tellStatus", &st, r.gid, []string{"status", "totalLength", "completedLength",
		"uploadLength", "downloadSpeed", "uploadSpeed", "connections", "seeder", "errorMessage"})
	if IsNotFound(err) {
		switch {
		case r.State == StateSeeding:
			r.State = StateCompleted
			s.setState(ctx, r.ID, StateCompleted, "")
		case r.Round > 0 && r.InfoHash != "": // its torrent is saved: plan the round again
			s.restartRound(ctx, r)
		default:
			s.fail(ctx, r, "aria2 lost the task")
		}
		return
	}
	if err != nil {
		return
	}
	// aria2 counts whole pieces, which can spill into unselected neighbours; cap at 100 %. Earlier
	// rounds count as done.
	done := num(st.CompletedLength)
	if r.roundBytes > 0 { // the round's files; aria2's own total is piece-aligned and can be larger
		done = min(done, r.roundBytes)
	} else if r.TotalBytes > 0 {
		done = min(done, r.TotalBytes)
	}
	done += r.doneBefore
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
	case r.State == StateDownloading && finished && len(remaining(r.files)) > 0:
		s.endRound(ctx, r)
	case r.State == StateDownloading && finished:
		if s.startImport(ctx, r) != nil {
			return // the files stay and the round stays finished-but-not-handed-over; tried again later (review #43)
		}
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

// roundPaths are the files a round's import reads: the round's own, and companions kept on disk
// from earlier rounds of the same folders (review #45). A download from before rounds (round 0)
// imports all its selected files.
func (r *row) roundPaths() []string {
	var paths []string
	for _, f := range r.files {
		p := filepath.Join(r.dir, filepath.FromSlash(f.Path))
		switch {
		case !f.Selected:
			continue
		case f.Round == r.Round:
		case companionFor(r.files, f, r.Round):
			if _, err := os.Stat(p); err != nil {
				continue
			}
		default:
			continue
		}
		paths = append(paths, p)
	}
	return paths
}

// restartRound gives a round's files back to the planner after aria2 lost its task (a restart
// before aria2 saved its session): the round starts over, added from the saved torrent.
func (s *Service) restartRound(ctx context.Context, r *row) {
	for i := range r.files {
		if r.files[i].Round == r.Round {
			r.files[i].Round = 0
		}
	}
	files, _ := json.Marshal(r.files)
	r.Round--
	r.State, r.gid, r.roundBytes, r.roundWork = StateQueued, "", 0, 0
	s.db.ExecContext(ctx, `UPDATE downloads SET state = ?, gid = '', files = ?, round = ?, round_bytes = 0, round_work = 0, done_bytes = done_before,
		note = 'aria2 lost the task; the round starts again', updated_at = ? WHERE id = ?`, StateQueued, string(files), r.Round, db.Now(), r.ID)
	s.poke()
}

// noImport marks the files of a round that had nothing to import (only scans, say): they are
// handed over all the same.
const noImport = -1

// importRetry is how long a failed hand-over to the importer waits before it is tried again.
var importRetry = time.Minute

// startImport hands the files of the round that just finished to the importer. The import batch
// and the download's link to it are written in one transaction, and only then does the download
// move on: if either fails, the files stay, nothing counts them as imported, and the hand-over is
// tried again on a later poll, after a restart too (review #43).
func (s *Service) startImport(ctx context.Context, r *row) error {
	if t, ok := s.importWait[r.ID]; ok && time.Now().Before(t) {
		return errors.New("waiting to retry the import")
	}
	round := func(f FileView) bool { return f.Selected && (f.Round == r.Round) }
	mark := func(batch int64) []FileView {
		files := slices.Clone(r.files)
		for i := range files {
			if round(files[i]) {
				files[i].Batch = batch
			}
		}
		return files
	}
	// No preview: the files were already chosen; the album can be tidied afterwards (P2-3).
	batch, n, err := s.imp.CreateBatchLinked(ctx, "download", r.Name, r.dir, r.roundPaths(), false, func(tx *sql.Tx, batch int64) error {
		files, _ := json.Marshal(mark(batch))
		_, err := tx.ExecContext(ctx, `UPDATE downloads SET import_batch_id = ?, files = ?, error = '', updated_at = ? WHERE id = ?`,
			batch, string(files), db.Now(), r.ID)
		return err
	})
	if errors.Is(err, importer.ErrNothingToImport) { // nothing the importer takes: handed over as done
		files, _ := json.Marshal(mark(noImport))
		if _, err = s.db.ExecContext(ctx, `UPDATE downloads SET files = ?, error = '', updated_at = ? WHERE id = ?`,
			string(files), db.Now(), r.ID); err == nil {
			r.files = mark(noImport)
			delete(s.importWait, r.ID)
			s.log.Info("round has nothing to import", "download", r.ID, "round", r.Round)
			return nil
		}
	}
	if err != nil {
		s.importWait[r.ID] = time.Now().Add(importRetry)
		s.db.ExecContext(ctx, `UPDATE downloads SET error = ?, updated_at = ? WHERE id = ?`,
			"handing the files to the importer failed; trying again: "+err.Error(), db.Now(), r.ID)
		s.log.Warn("hand download to the importer", "download", r.ID, "err", err)
		return err
	}
	delete(s.importWait, r.ID)
	r.ImportBatchID, r.files, r.Error = batch, mark(batch), ""
	s.log.Info("download complete; import queued", "download", r.ID, "round", r.Round, "batch", batch, "files", n)
	return nil
}

// endRound finishes a round that is not the last: the round's files are handed to the importer,
// then the aria2 task is removed (no seeding between rounds; the next round adds it again from the
// torrent kept beside the files). Its work space reservation is released for the import's own.
func (s *Service) endRound(ctx context.Context, r *row) {
	if _, err := s.torrentFor(ctx, r); err != nil { // the next round needs it; say so now
		s.log.Warn("torrent for later rounds", "download", r.ID, "err", err)
	}
	if s.startImport(ctx, r) != nil {
		return // the task stays as it is; tried again later (review #43)
	}
	s.aria.RPC.Call(ctx, "forceRemove", nil, r.gid)
	s.aria.RPC.Call(ctx, "removeDownloadResult", nil, r.gid)
	r.doneBefore += r.roundBytes
	r.State, r.gid, r.roundBytes, r.roundWork = StateImporting, "", 0, 0
	s.db.ExecContext(ctx, `UPDATE downloads SET state = ?, gid = '', round_bytes = 0, round_work = 0, done_before = ?, done_bytes = ?,
		down_speed = 0, up_speed = 0, peers = 0, note = ?, updated_at = ? WHERE id = ?`, StateImporting, r.doneBefore, r.doneBefore,
		fmt.Sprintf("round %d downloaded; importing it before the next round", r.Round), db.Now(), r.ID)
}

// afterRound waits for a round's import, then clears what the library has safely (keeping the
// companions later rounds need and files that did not make it into the library) and queues the
// next round.
func (s *Service) afterRound(ctx context.Context, r *row) {
	for _, f := range r.files { // the round's import must be the one that ended
		if f.Selected && f.Round == r.Round && f.Batch == 0 {
			if s.startImport(ctx, r) != nil {
				return
			}
			break
		}
	}
	if b := r.roundBatch(); b > 0 {
		var state string
		if err := s.db.QueryRowContext(ctx, `SELECT state FROM import_batches WHERE id = ?`, b).Scan(&state); err != nil ||
			(state != "done" && state != "canceled") {
			return
		}
	}
	keep := map[string]bool{}
	for _, b := range r.batches() {
		rows, err := s.db.QueryContext(ctx, `SELECT local_path, source_path FROM import_items i WHERE i.batch_id = ? AND `+importer.KeepsSource("i"), b)
		if err != nil {
			return
		}
		for rows.Next() {
			var p, src string
			if rows.Scan(&p, &src) == nil {
				keep[p], keep[src] = true, true
			}
		}
		rows.Close()
	}
	// Everything else goes, pieces spilled into later rounds' files too: the next round starts the
	// torrent afresh and fetches those whole.
	for _, f := range r.files {
		p := filepath.Join(r.dir, filepath.FromSlash(f.Path))
		if !keep[p] && !(f.Round > 0 && keepAfterRound(r.files, f)) {
			s.removeUnder(r.dir, p)
		}
	}
	for _, c := range []string{"*.aria2"} {
		if m, _ := filepath.Glob(filepath.Join(r.dir, c)); m != nil {
			for _, p := range m {
				os.Remove(p)
			}
		}
	}
	r.State = StateQueued
	s.db.ExecContext(ctx, `UPDATE downloads SET state = ?, note = ?, updated_at = ? WHERE id = ?`, StateQueued,
		fmt.Sprintf("round %d imported; the next round starts when there is room", r.Round), db.Now(), r.ID)
	s.poke()
}

// removeUnder deletes a file and the folders it leaves empty, up to root.
func (s *Service) removeUnder(root, p string) {
	if err := os.Remove(p); err != nil {
		return
	}
	root = filepath.Clean(root)
	for dir := filepath.Dir(p); strings.HasPrefix(dir, root+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			return
		}
	}
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
