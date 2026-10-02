package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/media"
)

// The import preview (P2-3): a batch in review shows how its files will be grouped into albums,
// and the user can correct that before anything is uploaded.

var (
	ErrNotInReview = errors.New("the import is not waiting for review")
	ErrBadOp       = errors.New("invalid change to the import plan")
)

type PreviewItem struct {
	ID         int64  `json:"id"`
	Path       string `json:"path"`
	Role       string `json:"role"`
	State      string `json:"state"`
	Error      string `json:"error,omitempty"`
	Plan       *Plan  `json:"plan,omitempty"`
	Format     string `json:"format,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Encoding   string `json:"encoding,omitempty"`   // the guess for fields stored without an encoding
	SameAudio  string `json:"same_audio,omitempty"` // a library track with the same decoded audio (D2 §6)
	Source     string `json:"source,omitempty"`     // converted | split: made by FFmpeg (P2-4)
}

type AlbumRef struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Tracks int    `json:"tracks"`
}

type PreviewGroup struct {
	Key         string        `json:"key"`
	Album       string        `json:"album"`
	AlbumArtist string        `json:"album_artist"`
	Date        string        `json:"date"`
	Kind        string        `json:"kind"` // music | spoken | mixed
	NewAlbum    bool          `json:"new_album"`
	Folders     []string      `json:"folders"`
	Existing    *AlbumRef     `json:"existing,omitempty"` // the library album the group will join
	Warnings    []string      `json:"warnings"`
	Items       []PreviewItem `json:"items"`
}

type Preview struct {
	BatchView
	Encoding   string         `json:"encoding"`  // chosen for the batch; "" means the automatic guess
	Encodings  []string       `json:"encodings"` // what can be chosen
	Detected   map[string]int `json:"detected"`  // files whose untagged text was guessed, by guess
	Groups     []PreviewGroup `json:"groups"`
	Standalone []PreviewItem  `json:"standalone"` // songs without an album
	Other      []PreviewItem  `json:"other"`      // skipped, failed and left-out files, sidecars, archives
}

// Preview describes a batch's plan. It works in any state; the edits need review.
func (im *Importer) Preview(ctx context.Context, batchID int64) (*Preview, error) {
	b, err := im.Batch(ctx, batchID)
	if err != nil || b == nil {
		return nil, err
	}
	b.Items = nil
	opts := im.options(ctx, batchID)
	p := &Preview{BatchView: *b, Encoding: opts.Encoding, Encodings: media.Encodings(), Detected: map[string]int{},
		Groups: []PreviewGroup{}, Standalone: []PreviewItem{}, Other: []PreviewItem{}}
	rows, err := im.db.QueryContext(ctx, `SELECT id, rel_path, role, state, error, coalesce(plan, ''), coalesce(info, ''), source_kind
		FROM import_items WHERE batch_id = ? ORDER BY rel_path`, batchID)
	if err != nil {
		return nil, err
	}
	type row struct {
		item       PreviewItem
		plan, info string
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.item.ID, &r.item.Path, &r.item.Role, &r.item.State, &r.item.Error, &r.plan, &r.info, &r.item.Source); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, r)
	}
	rows.Close()

	groups := map[string]*PreviewGroup{}
	legacy := map[string]bool{}
	for _, r := range list {
		it := r.item
		var info media.Info
		if r.info != "" && json.Unmarshal([]byte(r.info), &info) == nil {
			it.Format, it.DurationMS = info.Format, info.DurationMS
			if info.Encoding != "" && info.Encoding != media.EncUTF8 {
				it.Encoding = info.Encoding
				p.Detected[info.Encoding]++
			}
		}
		if r.item.Role != RoleAudio || r.item.State != "pending" || r.plan == "" {
			if r.plan != "" { // left out, but keep what it would have been
				var pl Plan
				if json.Unmarshal([]byte(r.plan), &pl) == nil {
					it.Plan = &pl
				}
			}
			p.Other = append(p.Other, it)
			continue
		}
		var pl Plan
		if json.Unmarshal([]byte(r.plan), &pl) != nil {
			p.Other = append(p.Other, it)
			continue
		}
		it.Plan = &pl
		if it.SameAudio, err = im.lib.TrackBySameAudio(ctx, info.AudioMD5); err != nil {
			return nil, err
		}
		if pl.Group == "" {
			p.Standalone = append(p.Standalone, it)
			continue
		}
		g := groups[pl.Group]
		if g == nil {
			g = &PreviewGroup{Key: pl.Group, Album: pl.Album, AlbumArtist: pl.AlbumArtist, Date: pl.Date, Kind: pl.Kind,
				NewAlbum: pl.NewAlbum, Folders: []string{}, Warnings: []string{}}
			groups[pl.Group] = g
		}
		if g.Kind != pl.Kind {
			g.Kind = "mixed"
		}
		if g.Date == "" {
			g.Date = pl.Date
		}
		if !slices.Contains(g.Folders, pl.Folder) {
			g.Folders = append(g.Folders, pl.Folder)
		}
		if it.Encoding != "" {
			legacy[pl.Group] = true
		}
		g.Items = append(g.Items, it)
	}
	var keys []string
	for k := range groups {
		keys = append(keys, k)
	}
	for _, k := range sortedGroups(keys) {
		g := groups[k]
		anchor := g.Items[0].Plan.Anchor
		if !g.NewAlbum && anchor.Album != "" {
			a, err := im.lib.AlbumByTags(ctx, anchor.Album, anchor.AlbumArtist)
			if err != nil {
				return nil, err
			}
			if a != nil {
				g.Existing = &AlbumRef{ID: a.ID, Title: a.Title, Tracks: a.Tracks}
			}
		}
		g.Warnings = groupWarnings(g, legacy[k] && opts.Encoding == "")
		p.Groups = append(p.Groups, *g)
	}
	return p, nil
}

// groupWarnings are the things worth a look before importing (codes the client words).
func groupWarnings(g *PreviewGroup, guessed bool) []string {
	w := []string{}
	if g.AlbumArtist == "" {
		w = append(w, "no_album_artist")
	}
	seen := map[[2]int]bool{}
	numbers := true
	lost := media.LooksLost(g.Album)
	same := false
	for _, it := range g.Items {
		k := [2]int{it.Plan.Disc, it.Plan.Track}
		if it.Plan.Track == 0 || seen[k] {
			numbers = false
		}
		seen[k] = true
		lost = lost || media.LooksLost(it.Plan.Title)
		same = same || it.SameAudio != ""
	}
	if !numbers {
		w = append(w, "track_numbers")
	}
	if lost {
		w = append(w, "lost_text")
	}
	if guessed {
		w = append(w, "guessed_encoding")
	}
	if same {
		w = append(w, "same_audio")
	}
	return w
}

// PlanOp is one edit of a batch's plan.
type PlanOp struct {
	Op          string  `json:"op"` // group | items | move | standalone | folders | encoding | exclude | include
	Group       string  `json:"group"`
	Into        string  `json:"into"` // move: a group key, "" for standalone songs, or "new"
	Items       []int64 `json:"items"`
	Album       *string `json:"album"`
	AlbumArtist *string `json:"album_artist"`
	Date        *string `json:"date"`
	Kind        *string `json:"kind"`
	Title       *string `json:"title"`
	Artist      *string `json:"artist"`
	Disc        *int    `json:"disc"`
	Track       *int    `json:"track"`
	NewAlbum    *bool   `json:"new_album"`
	Encoding    *string `json:"encoding"`
}

func badOp(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBadOp, fmt.Sprintf(format, args...))
}

func text(v *string, field string, required bool) (string, error) {
	s := strings.TrimSpace(*v)
	if required && s == "" {
		return "", badOp("%s cannot be empty", field)
	}
	if utf8.RuneCountInString(s) > 500 {
		return "", badOp("%s is too long", field)
	}
	return s, nil
}

// ApplyOp edits the plan of a batch in review.
func (im *Importer) ApplyOp(ctx context.Context, batchID int64, op PlanOp) error {
	var state string
	if err := im.db.QueryRowContext(ctx, `SELECT state FROM import_batches WHERE id = ?`, batchID).Scan(&state); err != nil {
		return err
	}
	if state != BatchReview {
		return ErrNotInReview
	}
	switch op.Op {
	case "encoding":
		enc := ""
		if op.Encoding != nil {
			enc = *op.Encoding
		}
		if enc != "" && !slices.Contains(media.Encodings(), enc) {
			return badOp("unknown encoding %q", enc)
		}
		b, _ := json.Marshal(batchOptions{Encoding: enc})
		if _, err := im.db.ExecContext(ctx, `UPDATE import_batches SET options = ? WHERE id = ?`, string(b), batchID); err != nil {
			return err
		}
		return im.replan(ctx, batchID) // re-reading the tags starts the plan over
	case "exclude", "include":
		from, to := "pending", StateExcluded
		if op.Op == "include" {
			from, to = StateExcluded, "pending"
		}
		for _, id := range op.Items {
			if _, err := im.db.ExecContext(ctx, `UPDATE import_items SET state = ?, updated_at = ? WHERE id = ? AND batch_id = ?
				AND role = ? AND state = ? AND plan IS NOT NULL`, to, db.Now(), id, batchID, RoleAudio, from); err != nil {
				return err
			}
		}
		plans, err := im.loadPlans(ctx, batchID)
		if err != nil {
			return err
		}
		settle(plans)
		return im.savePlans(ctx, plans)
	}

	plans, err := im.loadPlans(ctx, batchID)
	if err != nil {
		return err
	}
	pick := func(ids []int64) ([]*Plan, error) {
		var out []*Plan
		for _, id := range ids {
			i := slices.IndexFunc(plans, func(p planned) bool { return p.id == id })
			if i < 0 {
				return nil, badOp("file %d is not waiting in this import", id)
			}
			out = append(out, &plans[i].plan)
		}
		if len(out) == 0 {
			return nil, badOp("choose at least one file")
		}
		return out, nil
	}
	inGroup := func(g string) []*Plan {
		var out []*Plan
		for i := range plans {
			if g != "" && plans[i].plan.Group == g {
				out = append(out, &plans[i].plan)
			}
		}
		return out
	}

	switch op.Op {
	case "group": // album fields of a whole group
		ps := inGroup(op.Group)
		if len(ps) == 0 {
			return badOp("no group %q", op.Group)
		}
		for _, f := range []struct {
			v        *string
			name     string
			required bool
			set      func(p *Plan, s string)
		}{
			{op.Album, "album", true, func(p *Plan, s string) { p.Album = s }},
			{op.AlbumArtist, "album artist", false, func(p *Plan, s string) { p.AlbumArtist = s }},
			{op.Date, "date", false, func(p *Plan, s string) { p.Date = s }},
		} {
			if f.v == nil {
				continue
			}
			s, err := text(f.v, f.name, f.required)
			if err != nil {
				return err
			}
			for _, p := range ps {
				f.set(p, s)
			}
		}
		if op.Kind != nil {
			if *op.Kind != "music" && *op.Kind != "spoken" {
				return badOp("kind is music or spoken")
			}
			for _, p := range ps {
				p.Kind = *op.Kind
			}
		}
		if op.NewAlbum != nil {
			for _, p := range ps {
				p.NewAlbum = *op.NewAlbum
			}
		}
	case "items": // song fields
		ps, err := pick(op.Items)
		if err != nil {
			return err
		}
		for _, p := range ps {
			if op.Title != nil {
				if p.Title, err = text(op.Title, "title", true); err != nil {
					return err
				}
			}
			if op.Artist != nil {
				if p.Artist, err = text(op.Artist, "artist", false); err != nil {
					return err
				}
			}
			if op.Disc != nil {
				if *op.Disc < 1 || *op.Disc > 99 {
					return badOp("disc is 1 to 99")
				}
				p.Disc = *op.Disc
			}
			if op.Track != nil {
				if *op.Track < 0 || *op.Track > 999 {
					return badOp("track is 0 to 999")
				}
				p.Track = *op.Track
			}
			if op.Kind != nil {
				if *op.Kind != "music" && *op.Kind != "spoken" {
					return badOp("kind is music or spoken")
				}
				p.Kind = *op.Kind
			}
		}
	case "move": // files to another group, to a new group, or out of any album
		ps, err := pick(op.Items)
		if err != nil {
			return err
		}
		switch op.Into {
		case "":
			for _, p := range ps {
				p.Group, p.Album, p.NewAlbum = "", "", false
			}
		case "new":
			if op.Album == nil {
				return badOp("a new group needs an album name")
			}
			album, err := text(op.Album, "album", true)
			if err != nil {
				return err
			}
			artist := ""
			if op.AlbumArtist != nil {
				if artist, err = text(op.AlbumArtist, "album artist", false); err != nil {
					return err
				}
			}
			key, err := im.newGroupKey(ctx, batchID, plans)
			if err != nil {
				return err
			}
			for _, p := range ps {
				p.Group, p.Album, p.AlbumArtist, p.NewAlbum = key, album, artist, false
			}
		default:
			target := inGroup(op.Into)
			if len(target) == 0 {
				return badOp("no group %q", op.Into)
			}
			t := *target[0]
			for _, p := range ps {
				p.Group, p.Album, p.AlbumArtist, p.NewAlbum = t.Group, t.Album, t.AlbumArtist, t.NewAlbum
				if p.Date == "" {
					p.Date = t.Date
				}
			}
		}
	case "standalone": // the whole group becomes songs without an album
		ps := inGroup(op.Group)
		if len(ps) == 0 {
			return badOp("no group %q", op.Group)
		}
		for _, p := range ps {
			p.Group, p.Album, p.NewAlbum = "", "", false
		}
	case "folders": // every album folder is one album
		if err := im.byFolder(ctx, batchID, plans); err != nil {
			return err
		}
	default:
		return badOp("unknown operation %q", op.Op)
	}
	settle(plans)
	return im.savePlans(ctx, plans)
}

func (im *Importer) loadPlans(ctx context.Context, batchID int64) ([]planned, error) {
	rows, err := im.db.QueryContext(ctx, `SELECT id, rel_path, plan FROM import_items
		WHERE batch_id = ? AND role = ? AND state = 'pending' AND plan IS NOT NULL ORDER BY rel_path`, batchID, RoleAudio)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []planned
	for rows.Next() {
		var p planned
		var raw string
		if err := rows.Scan(&p.id, &p.rel, &raw); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(raw), &p.plan) == nil {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// newGroupKey is a group key no file of the batch uses, left-out files included.
func (im *Importer) newGroupKey(ctx context.Context, batchID int64, plans []planned) (string, error) {
	var top int
	err := im.db.QueryRowContext(ctx, `SELECT coalesce(max(CAST(substr(json_extract(plan, '$.group'), 2) AS INTEGER)), 0)
		FROM import_items WHERE batch_id = ? AND plan IS NOT NULL`, batchID).Scan(&top)
	for _, p := range plans {
		if n, _ := strconv.Atoi(strings.TrimPrefix(p.plan.Group, "g")); n > top {
			top = n
		}
	}
	return "g" + strconv.Itoa(top+1), err
}

// byFolder makes each album folder one album (plan §4 "每個子資料夾是一張專輯"): named by the album
// tag its files share, else by the folder. Files at the top of the batch stay as they are.
func (im *Importer) byFolder(ctx context.Context, batchID int64, plans []planned) error {
	folders := map[string][]int{}
	var order []string
	for i, p := range plans {
		f := p.plan.Folder
		if _, ok := folders[f]; !ok {
			order = append(order, f)
		}
		folders[f] = append(folders[f], i)
	}
	slices.Sort(order)
	for _, f := range order {
		if f == "." || f == "" {
			continue
		}
		idx := folders[f]
		album := ""
		shared := true
		var albumArtists, artists []string
		date := ""
		for _, i := range idx {
			p := plans[i].plan
			switch {
			case p.Tagged.Album == "":
				shared = false
			case album == "":
				album = p.Tagged.Album
			case album != p.Tagged.Album:
				shared = false
			}
			if p.AlbumArtist != "" && !slices.Contains(albumArtists, p.AlbumArtist) {
				albumArtists = append(albumArtists, p.AlbumArtist)
			}
			if p.Artist != "" && !slices.Contains(artists, p.Artist) {
				artists = append(artists, p.Artist)
			}
			if date == "" {
				date = p.Date
			}
		}
		if !shared || album == "" {
			album = path.Base(f)
		}
		artist := ""
		switch {
		case len(albumArtists) == 1:
			artist = albumArtists[0]
		case len(artists) == 1:
			artist = artists[0]
		case len(artists) > 1 || len(albumArtists) > 1:
			artist = "Various Artists"
		}
		key, err := im.newGroupKey(ctx, batchID, plans)
		if err != nil {
			return err
		}
		for _, i := range idx {
			p := &plans[i].plan
			p.Group, p.Album, p.AlbumArtist, p.NewAlbum = key, album, artist, false
			if p.Date == "" {
				p.Date = date
			}
		}
	}
	return nil
}

// Start runs a reviewed batch.
func (im *Importer) Start(ctx context.Context, batchID int64) error {
	r, err := im.db.ExecContext(ctx, `UPDATE import_batches SET state = ? WHERE id = ? AND state = ?`, BatchRunning, batchID, BatchReview)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotInReview
	}
	im.finishIfIdle(ctx, batchID)
	im.Wake()
	return nil
}

// Cancel drops a batch that has not started: nothing was uploaded; extracted archives and, for
// client uploads, the uploaded files are deleted.
func (im *Importer) Cancel(ctx context.Context, batchID int64) error {
	r, err := im.db.ExecContext(ctx, `UPDATE import_batches SET state = ?, finished_at = ? WHERE id = ? AND state IN (?, ?)`,
		BatchCanceled, db.Now(), batchID, BatchReview, BatchAnalyzing)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotInReview
	}
	im.db.ExecContext(ctx, `UPDATE import_items SET state = ?, updated_at = ? WHERE batch_id = ? AND state = 'pending'`,
		StateExcluded, db.Now(), batchID)
	os.RemoveAll(im.workDir(batchID))
	var kind, source string
	im.db.QueryRowContext(ctx, `SELECT kind, source FROM import_batches WHERE id = ?`, batchID).Scan(&kind, &source)
	if im.OnBatchDone != nil {
		im.OnBatchDone(ctx, kind, source, 0)
	}
	return nil
}
