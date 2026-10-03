package importer

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/media"
)

// Batch states (P2-3). A batch is analyzed first: archives are expanded and every file's tags are
// read, without hashing or uploading, and each file gets a plan. Batches made with preview wait in
// review for the user; the others run straight away.
const (
	BatchAnalyzing = "analyzing"
	BatchReview    = "review"
	BatchRunning   = "running"
	BatchDone      = "done"
	BatchCanceled  = "canceled"
)

// More item states.
const (
	StateExcluded = "excluded" // left out in the preview
	StateExpanded = "expanded" // a ZIP whose files became items of their own
)

// Item roles.
const (
	RoleAudio   = "audio"
	RoleSidecar = "sidecar"
	RoleZip     = "zip"
)

var sidecarExt = map[string]bool{".cue": true, ".log": true}

// Plan is what the import makes of one file.
type Plan struct {
	Group       string `json:"group"`  // album group within the batch; "" for a standalone track
	Folder      string `json:"folder"` // the album folder, relative to the batch (Disc 1/ ... belong to the folder above)
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	Album       string `json:"album"`
	AlbumArtist string `json:"album_artist"`
	Date        string `json:"date"`
	Disc        int    `json:"disc"`
	Track       int    `json:"track"`
	Kind        string `json:"kind"`
	NewAlbum    bool   `json:"new_album,omitempty"` // make a new album even if one was made from the same tags
	// Chosen marks a new album the user asked for in the preview: files imported before then go
	// into it too, instead of back to their earlier entries (review #21).
	Chosen bool `json:"chosen,omitempty"`

	// Tagged is what the file's own tags (and folder and file name) said, kept through preview edits:
	// later imports of the same file are matched by it.
	Tagged library.Tagged `json:"tagged"`
	// Anchor is the album identity of the whole group: the tags most of its files carry. The group's
	// first file finds or makes the album by it; the others follow that file.
	Anchor library.Tagged `json:"anchor"`
}

func (p *Plan) input() library.EntryInput {
	in := library.EntryInput{Title: p.Title, Artist: p.Artist, Album: p.Album, AlbumArtist: p.AlbumArtist, Date: p.Date,
		DiscNo: p.Disc, TrackNo: p.Track, Kind: p.Kind, NewAlbum: p.NewAlbum, Chosen: p.NewAlbum && p.Chosen}
	if p.Album != "" {
		t, a := p.Tagged, p.Anchor
		in.Tagged, in.AlbumTags = &t, &a
	}
	return in
}

func (im *Importer) workDir(batch int64) string {
	return filepath.Join(im.staging, "work", strconv.FormatInt(batch, 10))
}

// WorkCommitted is the staging space extracted archives hold (shared budget, plan §6).
func (im *Importer) WorkCommitted(context.Context) int64 {
	return dirSize(filepath.Join(im.staging, "work"))
}

func dirSize(root string) int64 {
	var n int64
	filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

func (im *Importer) nextAnalysis(ctx context.Context) int64 {
	var id int64
	im.db.QueryRowContext(ctx, `SELECT id FROM import_batches WHERE state = ? ORDER BY id LIMIT 1`, BatchAnalyzing).Scan(&id)
	return id
}

// analyze expands archives, reads every file's tags, plans the batch and moves it on to review or
// running. Problems with single files are recorded on them; an error return is a database failure.
func (im *Importer) analyze(ctx context.Context, batchID int64) error {
	var kind string
	if err := im.db.QueryRowContext(ctx, `SELECT kind FROM import_batches WHERE id = ?`, batchID).Scan(&kind); err != nil {
		return err
	}
	if err := im.fetchInboxLocal(ctx, batchID); err != nil { // inbox sidecars and archives are needed here
		return err
	}
	if err := im.expandZips(ctx, batchID, kind); err != nil {
		return err
	}
	if err := im.probeAll(ctx, batchID); err != nil {
		return err
	}
	// Disc images are cut by their CUE sheets first; what is left that players cannot play is
	// converted; then the new files are read like the others.
	if err := im.splitCues(ctx, batchID); err != nil {
		return err
	}
	if err := im.convertAll(ctx, batchID); err != nil {
		return err
	}
	if err := im.probeAll(ctx, batchID); err != nil {
		return err
	}
	if err := im.planNew(ctx, batchID); err != nil {
		return err
	}
	// A batch analyzed again for a retry was reviewed already: it runs.
	if _, err := im.db.ExecContext(ctx, `UPDATE import_batches
		SET state = CASE WHEN preview = 1 AND coalesce(json_extract(options, '$.rerun'), 0) = 0 THEN ? ELSE ? END,
		options = json_remove(coalesce(nullif(options, ''), '{}'), '$.rerun')
		WHERE id = ? AND state = ?`, BatchReview, BatchRunning, batchID, BatchAnalyzing); err != nil {
		return err
	}
	im.finishIfIdle(ctx, batchID)
	return nil
}

// expandZips turns each archive of the batch into items for the files it holds.
func (im *Importer) expandZips(ctx context.Context, batchID int64, kind string) error {
	rows, err := im.db.QueryContext(ctx, `SELECT id, local_path, rel_path FROM import_items
		WHERE batch_id = ? AND role = ? AND state = 'pending'`, batchID, RoleZip)
	if err != nil {
		return err
	}
	type zipItem struct {
		id        int64
		path, rel string
	}
	var zips []zipItem
	for rows.Next() {
		var z zipItem
		if err := rows.Scan(&z.id, &z.path, &z.rel); err != nil {
			rows.Close()
			return err
		}
		zips = append(zips, z)
	}
	rows.Close()
	for _, z := range zips {
		dir := filepath.Join(im.workDir(batchID), strconv.FormatInt(z.id, 10))
		os.RemoveAll(dir) // a restart mid-extraction starts the archive over
		files, err := im.expandZip(ctx, z.path, dir)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		under := dir + string(filepath.Separator)
		if err != nil {
			os.RemoveAll(dir)
			now := db.Now()
			im.db.ExecContext(ctx, `UPDATE import_items SET state = ?, error = ?, updated_at = ? WHERE id = ?`, StateFailed, err.Error(), now, z.id)
			// Files of it to be made again cannot be (review #65).
			im.db.ExecContext(ctx, `UPDATE import_items SET state = ?, error = ?, updated_at = ? WHERE batch_id = ? AND state = 'pending'
				AND source_kind = '' AND substr(local_path, 1, length(?)) = ?`, StateFailed, "its archive could not be unpacked: "+err.Error(),
				now, batchID, under, under)
			continue
		}
		// Unpacked again (a retry, review #65): the files go back to the items they were, keeping their
		// plans; those in the library already, or left out, are not imported again. A file converted
		// before is converted again, and a disc image cut before is cut again.
		type earlier struct {
			id    int64
			state string
		}
		before := map[string]earlier{}
		rows, err := im.db.QueryContext(ctx, `SELECT i.id, i.state, CASE WHEN i.source_kind = ? THEN i.source_path ELSE i.local_path END,
			EXISTS (SELECT 1 FROM import_items c WHERE c.batch_id = i.batch_id AND c.source_kind = ? AND c.source_path = i.local_path
				AND `+KeepsSource("c")+`)
			FROM import_items i WHERE i.batch_id = ? AND i.source_kind IN ('', ?)
			AND (substr(i.local_path, 1, length(?)) = ? OR substr(i.source_path, 1, length(?)) = ?)`,
			SourceConverted, SourceSplit, batchID, SourceConverted, under, under, under, under)
		if err != nil {
			return err
		}
		for rows.Next() {
			var e earlier
			var p string
			var unsavedCuts bool
			if err := rows.Scan(&e.id, &e.state, &p, &unsavedCuts); err != nil {
				rows.Close()
				return err
			}
			if e.state == StateSplit && !unsavedCuts {
				e.state = StatePublished // every song cut from it is in the library or let go of
			}
			before[p] = e
		}
		rows.Close()
		base := strings.TrimSuffix(z.rel, path.Ext(z.rel))
		tx, err := im.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		now := db.Now()
		for _, f := range files {
			role := RoleAudio
			ext := strings.ToLower(path.Ext(f))
			switch {
			case sidecarExt[ext]:
				role = RoleSidecar
			case !audioExt[ext]:
				continue // lyrics and pictures are read next to the audio
			}
			p := filepath.Join(dir, filepath.FromSlash(f))
			e, ok := before[p]
			switch {
			case ok && (saved(e.state) || e.state == StateExcluded || e.state == StateDiscarded):
			case ok:
				_, err = tx.ExecContext(ctx, `UPDATE import_items SET local_path = ?, source_path = '', source_kind = '', source_sha256 = '',
					source_size = 0, state = 'pending', error = '', info = NULL, updated_at = ? WHERE id = ?`, p, now, e.id)
			default:
				_, err = tx.ExecContext(ctx, `INSERT INTO import_items (batch_id, local_path, rel_path, state, role, temp, updated_at)
					VALUES (?, ?, ?, 'pending', ?, 1, ?)`, batchID, p, base+"/"+f, role, now)
			}
			if err != nil {
				tx.Rollback()
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE import_items SET state = ?, updated_at = ? WHERE id = ?`, StateExpanded, now, z.id); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if kind == "upload" {
			im.removeStaged(z.path) // the uploaded archive is a copy; its contents are in the work folder now
		}
	}
	return nil
}

// probeAll reads the tags of every audio file not read yet.
func (im *Importer) probeAll(ctx context.Context, batchID int64) error {
	rows, err := im.db.QueryContext(ctx, `SELECT id, local_path, drive_id, drive_size FROM import_items
		WHERE batch_id = ? AND role = ? AND state = 'pending' AND info IS NULL`, batchID, RoleAudio)
	if err != nil {
		return err
	}
	type pending struct {
		id          int64
		path, drive string
		size        int64
	}
	var list []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.path, &p.drive, &p.size); err != nil {
			rows.Close()
			return err
		}
		list = append(list, p)
	}
	rows.Close()
	for _, p := range list {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, msg := "pending", ""
		var info *media.Info
		var err error
		if p.path == "" && p.drive != "" {
			info, err = im.probeDrive(ctx, p.drive, p.size) // read in place, in the Drive inbox
		} else {
			info, err = probePath(p.path)
		}
		switch {
		case errors.Is(err, media.ErrUnknownFormat):
			state, msg = StateSkipped, "not a recognized audio file"
		case err != nil:
			state, msg = StateFailed, "cannot read audio: "+err.Error()
		case needsConversion(info) && im.FFmpeg == nil:
			state, msg = StateSkipped, errNoFFmpeg.Error()
		case !info.Playable && !needsConversion(info):
			state, msg = StateSkipped, unplayable(info)
		}
		var raw any
		if info != nil {
			b, _ := json.Marshal(info)
			raw = string(b)
		}
		if _, err := im.db.ExecContext(ctx, `UPDATE import_items SET state = ?, error = ?, info = ?, updated_at = ? WHERE id = ?`,
			state, msg, raw, db.Now(), p.id); err != nil {
			return err
		}
	}
	return nil
}

func probePath(p string) (*media.Info, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return media.Probe(f, st.Size())
}

func unplayable(info *media.Info) string {
	return fmt.Sprintf("%s (%s) is not supported: DSD and lossy formats other than MP3, AAC, Vorbis and Opus are skipped (D2)",
		info.Format, info.Codec)
}

// batchOptions are the per-batch choices made in the preview.
type batchOptions struct {
	Encoding string `json:"encoding,omitempty"` // re-decode legacy tag bytes with this encoding (D2 §4)
}

func (im *Importer) options(ctx context.Context, batchID int64) batchOptions {
	var raw string
	var o batchOptions
	if im.db.QueryRowContext(ctx, `SELECT options FROM import_batches WHERE id = ?`, batchID).Scan(&raw) == nil {
		json.Unmarshal([]byte(raw), &o)
	}
	return o
}

type probed struct {
	id   int64
	rel  string
	info media.Info
}

// loadProbed reads the tags saved for the batch's playable audio, decoded as the batch asks.
func (im *Importer) loadProbed(ctx context.Context, batchID int64) ([]probed, error) {
	enc := im.options(ctx, batchID).Encoding
	rows, err := im.db.QueryContext(ctx, `SELECT id, rel_path, info FROM import_items
		WHERE batch_id = ? AND role = ? AND state IN ('pending', ?) AND info IS NOT NULL ORDER BY rel_path`, batchID, RoleAudio, StateExcluded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []probed
	for rows.Next() {
		var p probed
		var raw string
		if err := rows.Scan(&p.id, &p.rel, &raw); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(raw), &p.info) != nil {
			continue
		}
		if enc != "" {
			p.info.Redecode(enc)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// planNew plans the files of the batch that have no plan yet: all of them the first time; after a
// retry, only songs that are new (cut from a disc image, say), whose groups are numbered after the
// batch's existing ones so they never join an album another group made.
func (im *Importer) planNew(ctx context.Context, batchID int64) error {
	list, err := im.loadProbed(ctx, batchID)
	if err != nil {
		return err
	}
	rows, err := im.db.QueryContext(ctx, `SELECT id, coalesce(json_extract(plan, '$.group'), '') FROM import_items
		WHERE batch_id = ? AND plan IS NOT NULL`, batchID)
	if err != nil {
		return err
	}
	planned := map[int64]bool{}
	last := 0
	for rows.Next() {
		var id int64
		var g string
		if err := rows.Scan(&id, &g); err != nil {
			rows.Close()
			return err
		}
		planned[id] = true
		if n, err := strconv.Atoi(strings.TrimPrefix(g, "g")); err == nil && n > last {
			last = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	fresh := list[:0]
	for _, p := range list {
		if !planned[p.id] {
			fresh = append(fresh, p)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	plans := defaultPlans(fresh)
	if last > 0 {
		for i := range plans {
			if g := plans[i].plan.Group; g != "" {
				n, _ := strconv.Atoi(strings.TrimPrefix(g, "g"))
				plans[i].plan.Group = "g" + strconv.Itoa(last+n)
			}
		}
	}
	settle(plans)
	return im.savePlans(ctx, plans)
}

// replan gives every file of the batch the plan its tags suggest, dropping preview edits.
func (im *Importer) replan(ctx context.Context, batchID int64) error {
	list, err := im.loadProbed(ctx, batchID)
	if err != nil {
		return err
	}
	plans := defaultPlans(list)
	settle(plans)
	return im.savePlans(ctx, plans)
}

// planned pairs an item with its plan while a batch is being planned or edited.
type planned struct {
	id   int64
	rel  string
	plan Plan
}

func (im *Importer) savePlans(ctx context.Context, plans []planned) error {
	tx, err := im.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range plans {
		b, _ := json.Marshal(p.plan)
		if _, err := tx.ExecContext(ctx, `UPDATE import_items SET plan = ? WHERE id = ?`, string(b), p.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// defaultPlans applies the evidence order of plan §4 to a batch: tags, then folders, then file
// names. Files of one album in one album folder share an album artist (decided like albumArtistFor);
// files with the same album and album artist form one group.
func defaultPlans(list []probed) []planned {
	type key struct{ root, album string }
	explicit := map[key]map[string]int{}
	performers := map[key]map[string]bool{}
	for _, p := range list {
		t := p.info.Tags
		if t.Album == "" {
			continue
		}
		k := key{albumRoot(path.Dir(p.rel)), t.Album}
		if t.AlbumArtist != "" {
			if explicit[k] == nil {
				explicit[k] = map[string]int{}
			}
			explicit[k][t.AlbumArtist]++
		} else if t.Artist != "" {
			if performers[k] == nil {
				performers[k] = map[string]bool{}
			}
			performers[k][t.Artist] = true
		}
	}
	decide := func(k key) string {
		if len(explicit[k]) > 0 {
			best, n := "", 0
			for a, c := range explicit[k] {
				if c > n || (c == n && a < best) {
					best, n = a, c
				}
			}
			return best
		}
		switch len(performers[k]) {
		case 0:
			return ""
		case 1:
			for a := range performers[k] {
				return a
			}
		}
		return "Various Artists"
	}
	// Files without an album tag take one from their album folder: when no file there has one, the
	// folder names the album, its album artist decided from the files' artists the same way; when
	// the others all share one album, they join it (a set's radio episodes on disc 1 whose disc 2
	// is tagged); when the folder has several albums, they stay standalone. Loose files (at the top
	// of what was imported, or in a folder whose name says nothing, like "Music") are not an album:
	// each keeps its own tags.
	albumsIn := map[string]map[string]bool{}
	folderArtists := map[string]map[string]bool{}
	for _, p := range list {
		root := albumRoot(path.Dir(p.rel))
		if albumsIn[root] == nil {
			albumsIn[root], folderArtists[root] = map[string]bool{}, map[string]bool{}
		}
		if p.info.Tags.Album != "" {
			albumsIn[root][p.info.Tags.Album] = true
		}
		if a := cmp.Or(p.info.Tags.AlbumArtist, p.info.Tags.Artist); a != "" {
			folderArtists[root][a] = true
		}
	}
	groups := map[string]string{}
	out := make([]planned, 0, len(list))
	for _, p := range list {
		in := entryInput(p.rel, &p.info)
		root := albumRoot(path.Dir(p.rel))
		tags := albumsIn[root]
		switch name := FolderAlbum(root); {
		case in.Album != "":
			if p.info.Tags.AlbumArtist == "" {
				in.AlbumArtist = decide(key{root, in.Album})
			}
		case name == "":
		case len(tags) == 1:
			for a := range tags {
				in.Album = a
			}
			in.AlbumArtist = decide(key{root, in.Album})
		case len(tags) == 0:
			in.Album, in.AlbumArtist = name, ""
			switch artists := folderArtists[root]; len(artists) {
			case 0:
			case 1:
				for a := range artists {
					in.AlbumArtist = a
				}
			default:
				in.AlbumArtist = "Various Artists"
			}
		}
		if in.Artist == "" && in.AlbumArtist != "Various Artists" {
			in.Artist = in.AlbumArtist // as for a file whose only artist tag is the album artist
		}
		kind := in.Kind
		if kind == "" {
			kind = "music"
		}
		plan := Plan{Folder: root, Title: in.Title, Artist: in.Artist, Album: in.Album, AlbumArtist: in.AlbumArtist, Date: in.Date,
			Disc: max(in.DiscNo, 1), Track: in.TrackNo, Kind: kind,
			Tagged: library.Tagged{Album: in.Album, AlbumArtist: in.AlbumArtist, Disc: max(in.DiscNo, 1), Track: in.TrackNo}}
		if in.Album != "" {
			gk := in.Album + "\x1f" + in.AlbumArtist
			if groups[gk] == "" {
				groups[gk] = "g" + strconv.Itoa(len(groups)+1)
			}
			plan.Group = groups[gk]
		}
		out = append(out, planned{p.id, p.rel, plan})
	}
	return out
}

// settle gives each group its anchor, the album identity most of its files carry (their tags, or
// the group's album when none has an album tag), and keeps two groups of one batch from landing in
// the same album: a later group with the same anchor makes a new album.
func settle(plans []planned) {
	type count struct {
		t library.Tagged
		n int
	}
	votes := map[string][]count{}
	var order []string
	for _, p := range plans {
		g := p.plan.Group
		if g == "" {
			continue
		}
		if _, ok := votes[g]; !ok {
			order = append(order, g)
		}
		t := library.Tagged{Album: p.plan.Tagged.Album, AlbumArtist: p.plan.Tagged.AlbumArtist}
		if t.Album == "" {
			t = library.Tagged{Album: p.plan.Album, AlbumArtist: p.plan.AlbumArtist}
		}
		found := false
		for i := range votes[g] {
			if votes[g][i].t == t {
				votes[g][i].n++
				found = true
			}
		}
		if !found {
			votes[g] = append(votes[g], count{t, 1})
		}
	}
	anchors := map[string]library.Tagged{}
	taken := map[library.Tagged]string{}
	forceNew := map[string]bool{}
	for _, g := range order {
		best := votes[g][0]
		for _, c := range votes[g][1:] {
			if c.n > best.n {
				best = c
			}
		}
		anchors[g] = best.t
		if other, ok := taken[best.t]; ok && other != g {
			forceNew[g] = true
		} else {
			taken[best.t] = g
		}
	}
	for i := range plans {
		p := &plans[i].plan
		if p.Group == "" {
			p.Anchor, p.NewAlbum = library.Tagged{}, false
			continue
		}
		p.Anchor = anchors[p.Group]
		if forceNew[p.Group] {
			p.NewAlbum = true
		}
	}
}

// groupAlbum is the album an earlier file of the same group went to, so the whole group lands in
// one album whatever its files' own tags say.
func (im *Importer) groupAlbum(ctx context.Context, batchID int64, group string) int64 {
	if group == "" {
		return 0
	}
	var id int64
	err := im.db.QueryRowContext(ctx, `SELECT e.album_id FROM import_items i JOIN album_entries e ON e.id = i.entry_id
		WHERE i.batch_id = ? AND i.role = ? AND json_extract(i.plan, '$.group') = ? ORDER BY i.id LIMIT 1`, batchID, RoleAudio, group).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		im.log.Warn("group album", "batch", batchID, "group", group, "err", err)
	}
	return id
}

// finishIfIdle closes a running batch that has nothing left to do (all files skipped, for example).
func (im *Importer) finishIfIdle(ctx context.Context, batchID int64) {
	res, err := im.db.ExecContext(ctx, `UPDATE import_batches SET state = ?, finished_at = ? WHERE id = ? AND state = ?
		AND NOT EXISTS (SELECT 1 FROM import_items WHERE batch_id = ? AND state IN ('pending', 'uploading'))`,
		BatchDone, db.Now(), batchID, BatchRunning, batchID)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		im.batchDone(ctx, batchID)
	}
}

// sortedGroups lists group keys in their natural order (g1, g2, ... g10).
func sortedGroups(keys []string) []string {
	sort.Slice(keys, func(i, j int) bool {
		a, _ := strconv.Atoi(strings.TrimPrefix(keys[i], "g"))
		b, _ := strconv.Atoi(strings.TrimPrefix(keys[j], "g"))
		return a < b
	})
	return keys
}
