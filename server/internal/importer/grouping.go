package importer

import (
	"context"
	"database/sql"
	"errors"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/library"
)

// Grouping is how the songs of a batch are put into albums when the user chose it for a download
// (review #82), instead of by their tags.
type Grouping struct {
	Mode   string `json:"mode"`   // GroupFolders or GroupCollection
	Title  string `json:"title"`  // the collection's
	Artist string `json:"artist"` // its album artist; Various Artists when empty
	// For a collection, worked out from the whole download (CollectionLayout) so every round agrees:
	// its sections in order (disc = index + 1), and the disc and track of each audio file of the
	// batch, by its path relative to the batch.
	Sections []string          `json:"sections,omitempty"`
	Slots    map[string][2]int `json:"slots,omitempty"`
}

const (
	GroupFolders    = "folders"    // every album folder is an album
	GroupCollection = "collection" // the whole download is one album, its folders the sections
)

// CollectionLayout places a collection's songs (review #82): its sections are the folders right
// below the top folder all its files share (the torrent's own), in natural order, songs loose at
// that level coming first; within a section, songs in the natural order of their paths, numbered
// from 1. paths are relative to the download, with slashes.
func CollectionLayout(paths []string) (sections []string, slots map[string][2]int) {
	var audio []string
	for _, p := range paths {
		if audioExt[strings.ToLower(path.Ext(p))] {
			audio = append(audio, p)
		}
	}
	slices.SortFunc(audio, NaturalCompare)
	top := ""
	if len(audio) > 0 {
		if first, _, ok := strings.Cut(audio[0], "/"); ok {
			top = first + "/"
			for _, p := range audio {
				if !strings.HasPrefix(p, top) {
					top = ""
					break
				}
			}
		}
	}
	section := func(p string) string {
		rest := strings.TrimPrefix(p, top)
		if s, _, ok := strings.Cut(rest, "/"); ok {
			return s
		}
		return ""
	}
	for _, p := range audio {
		if s := section(p); !slices.Contains(sections, s) {
			sections = append(sections, s)
		}
	}
	slices.SortFunc(sections, func(a, b string) int {
		if (a == "") != (b == "") {
			return map[bool]int{true: -1, false: 1}[a == ""]
		}
		return NaturalCompare(a, b)
	})
	slots = map[string][2]int{}
	count := map[string]int{}
	for _, p := range audio {
		s := section(p)
		count[s]++
		slots[p] = [2]int{slices.Index(sections, s) + 1, count[s]}
	}
	return sections, slots
}

// group applies the batch's grouping to new plans, after settle: they are the user's choice.
func (im *Importer) group(ctx context.Context, batchID int64, plans []planned) error {
	g := im.options(ctx, batchID).Grouping
	if g == nil {
		return nil
	}
	switch g.Mode {
	case GroupFolders:
		if err := im.byFolder(ctx, batchID, plans); err != nil {
			return err
		}
		if err := im.joinFolders(ctx, batchID, plans); err != nil {
			return err
		}
		settle(plans)
	case GroupCollection:
		artist := strings.TrimSpace(g.Artist)
		if artist == "" {
			artist = "Various Artists"
		}
		title := strings.TrimSpace(g.Title)
		if title == "" {
			return nil
		}
		for i := range plans {
			p := &plans[i].plan
			// One album for the whole download, found again by every round (album_scopes): the scope is
			// the download and the collection, not a folder.
			p.Group, p.Folder, p.Album, p.AlbumArtist, p.NewAlbum, p.Chosen, p.DerivedArtist = "g1", "", title, artist, false, false, false
			p.Anchor = library.Tagged{Album: title, AlbumArtist: artist}
			disc, track := 0, p.Track
			if s, ok := g.Slots[filepath.ToSlash(plans[i].rel)]; ok {
				disc, track = s[0], s[1]
			} else { // made here (cut from an image): the section its folder is
				for j, name := range g.Sections {
					if name != "" && slices.Contains(strings.Split(path.Dir(filepath.ToSlash(plans[i].rel)), "/"), name) {
						disc = j + 1
					}
				}
			}
			if disc > 0 {
				p.Disc, p.Track = disc, track
				if disc <= len(g.Sections) {
					p.Section = g.Sections[disc-1]
				}
			}
		}
	}
	return nil
}

// joinFolders marks the plans byFolder grouped as their folder's album, and takes in what earlier
// rounds of the download made of the same folders (review #86): the title of a tag all the folder's
// songs share holds only while every round's songs share it, else the folder names the album; album
// artists that differ between rounds make Various Artists. One import of the whole folder decides
// the same, so the album does not depend on which round brought which song.
func (im *Importer) joinFolders(ctx context.Context, batchID int64, plans []planned) error {
	var root string
	if err := im.db.QueryRowContext(ctx, `SELECT root FROM import_batches WHERE id = ?`, batchID).Scan(&root); err != nil {
		return err
	}
	type earlier struct {
		title, artist string
		found         bool
	}
	seen := map[string]earlier{}
	for i := range plans {
		p := &plans[i].plan
		if p.Group == "" || p.Folder == "" || p.Folder == "." {
			continue
		}
		p.Folders = true
		e, ok := seen[p.Folder]
		if !ok && root != "" {
			err := im.db.QueryRowContext(ctx, `SELECT title, artist FROM album_scopes WHERE scope = ?`, folderScope(root, p.Folder)).
				Scan(&e.title, &e.artist)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			e.found = err == nil
			seen[p.Folder] = e
		}
		if !e.found {
			continue
		}
		if e.title != "" && e.title != p.Album {
			p.Album = path.Base(p.Folder)
		}
		switch {
		case e.artist == "" || e.artist == p.AlbumArtist:
		case p.AlbumArtist == "":
			p.AlbumArtist = e.artist
		default:
			p.AlbumArtist = "Various Artists"
		}
	}
	return nil
}

// CollectionScope names a download's collection in album_scopes: the download's folder and the
// collection's title, as scopeOf names it for the collection's songs.
func CollectionScope(root, title string) string { return root + "\x1f\x1f" + strings.TrimSpace(title) }

// ScopeAlbum is the album a scope went to, following merges; 0 when none (or gone).
func (im *Importer) ScopeAlbum(ctx context.Context, scope string) (int64, error) {
	var id int64
	err := im.db.QueryRowContext(ctx, `SELECT album_id FROM album_scopes WHERE scope = ?`, scope).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	id, _, err = im.lib.AlbumNow(ctx, id)
	return id, err
}

// SetScopeAlbum records the album of a scope (an organized collection: its later rounds join it).
func (im *Importer) SetScopeAlbum(ctx context.Context, scope string, albumID int64, artist string) error {
	_, err := im.db.ExecContext(ctx, `INSERT INTO album_scopes (scope, album_id, artist, derived, created_at) VALUES (?, ?, ?, 0, ?)
		ON CONFLICT (scope) DO UPDATE SET album_id = excluded.album_id, artist = excluded.artist, derived = 0`, scope, albumID, artist, db.Now())
	return err
}
