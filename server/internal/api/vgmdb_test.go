package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/HHim8826/kanade/server/internal/identify"
	"github.com/HHim8826/kanade/server/internal/library"
)

// An album read from a pasted VGMdb page is compared with an album of the library, and the picked
// changes are applied as one action of its own source.
func TestVGMdbProposeAndApply(t *testing.T) {
	ctx := context.Background()
	s, h := newTestServer(t)
	token := loginToken(t, s, h)
	var albumID int64
	for i, name := range []string{"DUE01", "DUE02"} {
		a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: name, Size: 1, Format: "mp3", Codec: "mp3", DurationMS: 1_600_000})
		s.lib.MarkVerified(ctx, a.ID, "drive-"+name)
		r, _ := s.lib.Publish(ctx, a.ID, library.EntryInput{Title: name, Album: "ARIA The STATION Due COUR.1", TrackNo: i + 1, Kind: "spoken"})
		s.db.QueryRow(`SELECT album_id FROM album_entries WHERE id = ?`, r.EntryID).Scan(&albumID)
	}
	path := fmt.Sprintf("/api/v1/albums/%d/vgmdb", albumID)
	album := identify.VGMdbAlbum{ID: 9, Title: "ARIA The STATION Due COUR.1", AlbumArtist: "葉月絵理乃", Date: "2007-01-24", Catalog: "FCCC-0001",
		Discs: []identify.VGMdbDisc{{Tracks: []identify.VGMdbTrack{{Title: "第1回", LengthMS: 1_600_000}, {Title: "第2回", LengthMS: 1_600_000}}}}}

	bad := album
	bad.Cover = "https://example.com/albums/x.jpg"
	if rec := do(t, h, "POST", path, token, map[string]any{"album": bad}); rec.Code != http.StatusBadRequest {
		t.Fatalf("other cover host: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/api/v1/albums/999/vgmdb", token, map[string]any{"album": album}); rec.Code != http.StatusNotFound {
		t.Fatalf("no album: %d", rec.Code)
	}
	rec := do(t, h, "POST", path, token, map[string]any{"album": album})
	var p identify.Proposal
	json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != 200 || p.Matched != 2 || p.Cover || len(p.Changes) != 7 { // artist, date, catalog; 2 titles, 2 artists
		t.Fatalf("proposal %d %s", rec.Code, rec.Body)
	}
	var keys []string
	for _, c := range p.Changes {
		if c.Field == "title" || c.Field == "catalog" {
			keys = append(keys, c.Key)
		}
	}
	rec = do(t, h, "POST", path+"/apply", token, map[string]any{"album": album, "keys": keys})
	var g struct{ Group int64 }
	json.Unmarshal(rec.Body.Bytes(), &g)
	if rec.Code != 200 || g.Group == 0 {
		t.Fatalf("apply %d %s", rec.Code, rec.Body)
	}
	d, _ := s.lib.Album(ctx, albumID)
	if d.Catalog != "FCCC-0001" || d.AlbumArtist != "" || d.Entries[0].Title != "第1回" || d.Entries[1].Title != "第2回" {
		t.Fatalf("album %+v", d)
	}
	if eg, _ := s.lib.EditGroup(ctx, g.Group); eg == nil || eg.Source != library.SourceVGMdb {
		t.Fatalf("edit group %+v", eg)
	}
}

func TestFolderAlbumsAPI(t *testing.T) {
	s, h := newTestServer(t)
	token := loginToken(t, s, h)
	if rec := do(t, h, "GET", "/api/v1/organize/folders", token, nil); rec.Code != 200 || rec.Body.String() != "[]\n" {
		t.Fatalf("list %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/api/v1/organize/folders", token, map[string]any{"folders": []any{}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("nothing chosen: %d", rec.Code)
	}
	rec := do(t, h, "POST", "/api/v1/organize/folders", token, map[string]any{"folders": []map[string]string{{"folder": "gone", "title": "x"}}})
	if rec.Code != 200 || rec.Body.String() != `{"group":0}`+"\n" {
		t.Fatalf("unknown folder %d %s", rec.Code, rec.Body)
	}
}
