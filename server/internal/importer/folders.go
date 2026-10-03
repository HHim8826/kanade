package importer

import (
	"cmp"
	"context"
	"encoding/json"
	"path"
	"slices"
	"strings"

	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/media"
)

// Folder albums for files imported before the folder rule (defaultPlans): standalone tracks without
// an album tag whose import put them in an album folder that names the album (no file there has an
// album tag) or whose other files all went to one album.

// FolderGroup is the standalone tracks of one album folder.
type FolderGroup struct {
	Folder      string         `json:"folder"` // as the import saw it, e.g. ARIA/Drama CD/ARIA The STATION Due COUR.1
	Title       string         `json:"title"`
	AlbumArtist string         `json:"album_artist"`
	AlbumID     int64          `json:"album_id,omitempty"` // the album the tracks go to, when it exists
	Join        bool           `json:"join"`               // AlbumID is where the folder's tagged files went
	Tracks      []FolderTrack  `json:"tracks"`
	tagged      library.Tagged // the identity the rule gives a new album
}

type FolderTrack struct {
	TrackID    int64  `json:"track_id"`
	AssetID    int64  `json:"-"`
	Title      string `json:"title"`
	File       string `json:"file"`
	Disc       int    `json:"disc"`
	Track      int    `json:"track"`
	DurationMS int64  `json:"duration_ms"`
}

// FolderGroups lists the folders that would make albums of standalone tracks, by folder.
func (im *Importer) FolderGroups(ctx context.Context) ([]FolderGroup, error) {
	// Every audio file imported, for the folders that have album tags somewhere, and the latest
	// import of each standalone track.
	rows, err := im.db.QueryContext(ctx, `SELECT i.id, i.rel_path, coalesce(i.info, ''), coalesce(i.track_id, 0), coalesce(i.asset_id, 0),
		coalesce(b.options, ''), coalesce((SELECT e.album_id FROM album_entries e WHERE e.id = i.entry_id), 0),
		coalesce(t.title, ''), coalesce(a.duration_ms, 0),
		t.id IS NOT NULL AND a.id IS NOT NULL AND i.id = (SELECT max(x.id) FROM import_items x WHERE x.track_id = t.id AND x.info IS NOT NULL)
		FROM import_items i JOIN import_batches b ON b.id = i.batch_id
		LEFT JOIN tracks t ON t.id = i.track_id AND NOT EXISTS (SELECT 1 FROM album_entries e WHERE e.track_id = t.id)
		LEFT JOIN assets a ON a.id = i.asset_id AND a.state = 'verified'
			AND EXISTS (SELECT 1 FROM track_assets ta WHERE ta.track_id = t.id AND ta.asset_id = a.id)
		WHERE i.role = ? AND i.info IS NOT NULL ORDER BY i.rel_path, i.id`, RoleAudio)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	albumsIn := map[string]map[string]bool{} // album tags of each folder
	wentTo := map[string]map[int64]int{}     // where the folder's tagged files are
	byFolder := map[string]*FolderGroup{}
	artists := map[string]map[string]bool{}
	var order []string
	for rows.Next() {
		var id, trackID, assetID, entryAlbum, duration int64
		var rel, raw, opts, title string
		var standalone bool
		if err := rows.Scan(&id, &rel, &raw, &trackID, &assetID, &opts, &entryAlbum, &title, &duration, &standalone); err != nil {
			return nil, err
		}
		var info media.Info
		if json.Unmarshal([]byte(raw), &info) != nil {
			continue
		}
		var o batchOptions
		if json.Unmarshal([]byte(opts), &o) == nil && o.Encoding != "" {
			info.Redecode(o.Encoding)
		}
		root := albumRoot(path.Dir(rel))
		if info.Tags.Album != "" {
			if albumsIn[root] == nil {
				albumsIn[root], wentTo[root] = map[string]bool{}, map[int64]int{}
			}
			albumsIn[root][info.Tags.Album] = true
			if entryAlbum != 0 {
				wentTo[root][entryAlbum]++
			}
			continue
		}
		if !standalone {
			continue
		}
		g := byFolder[root]
		if g == nil {
			g = &FolderGroup{Folder: root, Title: FolderAlbum(root)}
			byFolder[root] = g
			artists[root] = map[string]bool{}
			order = append(order, root)
		}
		if slices.ContainsFunc(g.Tracks, func(t FolderTrack) bool { return t.TrackID == trackID }) {
			continue
		}
		in := entryInput(rel, &info)
		g.Tracks = append(g.Tracks, FolderTrack{TrackID: trackID, AssetID: assetID, Title: title, File: path.Base(rel),
			Disc: max(in.DiscNo, 1), Track: in.TrackNo, DurationMS: duration})
		if a := cmp.Or(info.Tags.AlbumArtist, info.Tags.Artist); a != "" {
			artists[root][a] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []FolderGroup{}
	for _, root := range order {
		g := byFolder[root]
		switch {
		case len(albumsIn[root]) == 1: // join the album the folder's tagged files went to
			best, n := int64(0), 0
			for id, c := range wentTo[root] {
				if c > n || (c == n && id < best) {
					best, n = id, c
				}
			}
			if best == 0 {
				continue
			}
			a, err := im.lib.Album(ctx, best)
			if err != nil {
				return nil, err
			}
			for hops := 0; a != nil && a.MergedInto != 0 && hops < 10; hops++ {
				if a, err = im.lib.Album(ctx, a.MergedInto); err != nil {
					return nil, err
				}
			}
			if a == nil {
				continue
			}
			g.Title, g.AlbumArtist, g.AlbumID, g.Join = a.Title, a.AlbumArtist, a.ID, true
			sortFolderTracks(g.Tracks)
			out = append(out, *g)
			continue
		case len(albumsIn[root]) > 1 || g.Title == "":
			continue
		}
		switch len(artists[root]) {
		case 0:
		case 1:
			for a := range artists[root] {
				g.AlbumArtist = a
			}
		default:
			g.AlbumArtist = "Various Artists"
		}
		g.tagged = library.Tagged{Album: g.Title, AlbumArtist: g.AlbumArtist}
		sortFolderTracks(g.Tracks)
		album, err := im.lib.AlbumByTags(ctx, g.Title, g.AlbumArtist)
		if err != nil {
			return nil, err
		}
		if album != nil {
			g.AlbumID = album.ID
		}
		out = append(out, *g)
	}
	return out, nil
}

func sortFolderTracks(tracks []FolderTrack) {
	slices.SortFunc(tracks, func(a, b FolderTrack) int {
		return cmp.Or(cmp.Compare(a.Disc, b.Disc), cmp.Compare(a.Track, b.Track), naturalCompare(a.File, b.File))
	})
}

// FolderChoice is a folder to make an album of, with the title and album artist the user settled on.
type FolderChoice struct {
	Folder      string `json:"folder"`
	Title       string `json:"title"`
	AlbumArtist string `json:"album_artist"`
}

// MakeFolderAlbums makes the chosen folders' albums as one undoable action.
func (im *Importer) MakeFolderAlbums(ctx context.Context, choices []FolderChoice) (int64, error) {
	groups, err := im.FolderGroups(ctx)
	if err != nil {
		return 0, err
	}
	var albums []library.NewAlbum
	for _, c := range choices {
		i := slices.IndexFunc(groups, func(g FolderGroup) bool { return g.Folder == c.Folder })
		if i < 0 {
			continue // sorted out meanwhile
		}
		g := groups[i]
		a := library.NewAlbum{Title: cmp.Or(strings.TrimSpace(c.Title), g.Title), AlbumArtist: strings.TrimSpace(c.AlbumArtist), Tagged: g.tagged}
		if g.Join {
			a = library.NewAlbum{AlbumID: g.AlbumID}
		}
		for _, t := range g.Tracks {
			a.Entries = append(a.Entries, library.NewEntry{TrackID: t.TrackID, AssetID: t.AssetID, Disc: t.Disc, Track: t.Track})
		}
		albums = append(albums, a)
	}
	if len(albums) == 0 {
		return 0, nil
	}
	return im.lib.MakeAlbums(ctx, albums)
}

// naturalCompare orders names with their numbers by value: "due9" before "due10".
func naturalCompare(a, b string) int {
	for a != "" && b != "" {
		da, db := digitsPrefix(a), digitsPrefix(b)
		if da != "" && db != "" {
			na, nb := strings.TrimLeft(da, "0"), strings.TrimLeft(db, "0")
			if c := cmp.Or(cmp.Compare(len(na), len(nb)), strings.Compare(na, nb)); c != 0 {
				return c
			}
			a, b = a[len(da):], b[len(db):]
			continue
		}
		if c := cmp.Compare(strings.ToLower(a[:1]), strings.ToLower(b[:1])); c != 0 {
			return c
		}
		a, b = a[1:], b[1:]
	}
	return cmp.Compare(len(a), len(b))
}

func digitsPrefix(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}
