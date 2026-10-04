package library

import (
	"context"
	"errors"
	"testing"
)

// Bookmarks mark places in a song, listed by place, renamed and deleted; one whose file is no
// longer the song's (missing, or cut again to another length) says it moved instead of pointing
// at the same second of other audio; a deleted song takes its bookmarks along (review #98).
func TestBookmarks(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := fixture{t, s}
	r := f.song("a", "Drama", "Album", 1, 1) // a 60 s file
	var asset int64
	s.db.QueryRow(`SELECT asset_id FROM album_entries WHERE id = ?`, r.EntryID).Scan(&asset)
	later, err := s.AddBookmark(ctx, asset, 40_000, "下次從這裡", "")
	if err != nil || later.Asset == nil || later.Moved || later.Track == nil {
		t.Fatalf("add: %+v %v", later, err)
	}
	if _, err := s.AddBookmark(ctx, asset, 10_000, " 有趣的對話 ", "第二段"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddBookmark(ctx, asset, 90_000, "past the end", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("outside the song: %v", err)
	}
	if _, err := s.AddBookmark(ctx, asset, 1000, " ", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no name: %v", err)
	}
	list, _ := s.Bookmarks(ctx, r.TrackID)
	if len(list) != 2 || list[0].Name != "有趣的對話" || list[0].PositionMS != 10_000 || list[1].ID != later.ID {
		t.Fatalf("by place: %+v", list)
	}
	if err := s.UpdateBookmark(ctx, later.ID, "結局前", "記得"); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.Bookmarks(ctx, 0); len(all) != 2 || all[0].Name != "結局前" && all[1].Name != "結局前" {
		t.Fatalf("all: %+v", all)
	}

	// The file cut again to another length: the bookmark moved.
	s.db.Exec(`UPDATE assets SET duration_ms = 120000 WHERE id = ?`, asset)
	if list, _ := s.Bookmarks(ctx, r.TrackID); !list[0].Moved || list[0].Asset != nil {
		t.Fatalf("after the file changed: %+v", list[0])
	}
	s.db.Exec(`UPDATE assets SET duration_ms = 60000, state = 'missing' WHERE id = ?`, asset)
	if list, _ := s.Bookmarks(ctx, r.TrackID); !list[0].Moved {
		t.Fatalf("with the file missing: %+v", list[0])
	}

	if err := s.DeleteBookmark(ctx, later.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBookmark(ctx, later.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted twice: %v", err)
	}
	if _, err := s.DeleteTrack(ctx, r.TrackID); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.Bookmarks(ctx, 0); len(all) != 0 {
		t.Fatalf("after deleting the song: %+v", all)
	}
}
