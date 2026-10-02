package library

import (
	"context"
	"errors"
	"testing"
)

func publish(t *testing.T, s *Store, sha string, in EntryInput) PublishResult {
	t.Helper()
	r, err := s.Publish(context.Background(), verifiedAsset(t, s, sha), in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFavorites(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	r := publish(t, s, "f1", EntryInput{Title: "Song", Artist: "A", Album: "Al", AlbumArtist: "A", TrackNo: 1})
	var albumID int64
	s.db.QueryRow(`SELECT album_id FROM album_entries WHERE id = ?`, r.EntryID).Scan(&albumID)

	if err := s.SetFavorite(ctx, "track", r.TrackID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFavorite(ctx, "track", r.TrackID, true); err != nil { // twice is fine
		t.Fatal(err)
	}
	if err := s.SetFavorite(ctx, "album", albumID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFavorite(ctx, "track", 999, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown track: %v", err)
	}
	ids, _ := s.FavoriteIDs(ctx)
	if len(ids.Tracks) != 1 || ids.Tracks[0] != r.TrackID || len(ids.Albums) != 1 {
		t.Fatalf("ids = %+v", ids)
	}
	f, err := s.Favorites(ctx)
	if err != nil || len(f.Tracks) != 1 || f.Tracks[0].Title != "Song" || len(f.Albums) != 1 || f.Albums[0].Title != "Al" {
		t.Fatalf("favorites = %+v %v", f, err)
	}
	s.SetFavorite(ctx, "track", r.TrackID, false)
	if ids, _ := s.FavoriteIDs(ctx); len(ids.Tracks) != 0 {
		t.Fatalf("unfavorite kept %+v", ids)
	}
}

func TestPlaylists(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := publish(t, s, "p1", EntryInput{Title: "One", Album: "First", AlbumArtist: "X", TrackNo: 1})
	b := publish(t, s, "p2", EntryInput{Title: "Two", Album: "First", AlbumArtist: "X", TrackNo: 2})
	single := publish(t, s, "p3", EntryInput{Title: "Alone"})
	var first int64
	s.db.QueryRow(`SELECT album_id FROM album_entries WHERE id = ?`, a.EntryID).Scan(&first)

	if _, err := s.CreatePlaylist(ctx, "  ", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank name: %v", err)
	}
	id, err := s.CreatePlaylist(ctx, "Drive", "")
	if err != nil {
		t.Fatal(err)
	}
	// The same song twice is allowed; a bogus album falls back to the track's own.
	n, err := s.AddToPlaylist(ctx, id, []PlaylistAdd{{TrackID: a.TrackID, AlbumID: first}, {TrackID: single.TrackID},
		{TrackID: b.TrackID, AlbumID: 777}, {TrackID: a.TrackID}, {TrackID: 999}})
	if err != nil || n != 4 {
		t.Fatalf("added %d, %v", n, err)
	}
	d, err := s.Playlist(ctx, id)
	if err != nil || len(d.Items) != 4 {
		t.Fatalf("playlist = %+v %v", d, err)
	}
	titles := func(d *PlaylistDetail) (out []string) {
		for _, it := range d.Items {
			out = append(out, it.Title)
		}
		return out
	}
	if got := titles(d); got[0] != "One" || got[1] != "Alone" || got[2] != "Two" || got[3] != "One" {
		t.Fatalf("order %v", got)
	}
	if d.Items[2].AlbumID != first || d.Items[1].AlbumID != 0 || d.DurationMS != 4*60000 || d.Tracks != 4 {
		t.Fatalf("items %+v duration %d", d.Items, d.DurationMS)
	}

	// Reorder needs exactly the playlist's items.
	ids := []int64{d.Items[3].ItemID, d.Items[2].ItemID, d.Items[1].ItemID, d.Items[0].ItemID}
	if err := s.ReorderPlaylist(ctx, id, ids[:3]); !errors.Is(err, ErrInvalid) {
		t.Fatalf("partial reorder: %v", err)
	}
	if err := s.ReorderPlaylist(ctx, id, []int64{ids[0], ids[0], ids[1], ids[2]}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate reorder: %v", err)
	}
	if err := s.ReorderPlaylist(ctx, id, ids); err != nil {
		t.Fatal(err)
	}
	if err := s.RemovePlaylistItem(ctx, id, ids[1]); err != nil {
		t.Fatal(err)
	}
	d, _ = s.Playlist(ctx, id)
	if got := titles(d); len(got) != 3 || got[0] != "One" || got[1] != "Alone" || got[2] != "One" {
		t.Fatalf("after reorder and remove: %v", got)
	}
	list, _ := s.Playlists(ctx)
	if len(list) != 1 || list[0].Tracks != 3 || list[0].Name != "Drive" {
		t.Fatalf("list %+v", list)
	}
	if err := s.UpdatePlaylist(ctx, id, "Night drive", "late"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePlaylist(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePlaylist(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestLyricsPrecedence(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	r := publish(t, s, "l1", EntryInput{Title: "Song"})
	get := func() *Lyrics {
		l, err := s.Lyrics(ctx, r.TrackID)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	s.SetLyrics(ctx, r.TrackID, LyricsEmbedded, "plain words")
	if l := get(); l == nil || l.Synced || l.Source != LyricsEmbedded {
		t.Fatalf("embedded: %+v", l)
	}
	s.SetLyrics(ctx, r.TrackID, LyricsLRC, "[00:01.00]timed\r\n[00:02.50]words")
	if l := get(); !l.Synced || l.Source != LyricsLRC || l.Text != "[00:01.00]timed\n[00:02.50]words" {
		t.Fatalf("lrc should replace plain: %+v", l)
	}
	if changed, _ := s.SetLyrics(ctx, r.TrackID, LyricsEmbedded, "plain again"); changed {
		t.Fatal("plain text replaced synced lyrics")
	}
	s.SetLyrics(ctx, r.TrackID, LyricsManual, "my own")
	if changed, _ := s.SetLyrics(ctx, r.TrackID, LyricsLRC, "[00:01.00]import"); changed || get().Text != "my own" {
		t.Fatalf("import replaced manual lyrics: %+v", get())
	}
	s.SetLyrics(ctx, r.TrackID, LyricsManual, "")
	if l := get(); l != nil {
		t.Fatalf("manual delete kept %+v", l)
	}
}

func TestHistoryAndTop(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	r := publish(t, s, "h1", EntryInput{Title: "Song", Album: "Al", AlbumArtist: "A"})
	var asset int64
	s.db.QueryRow(`SELECT asset_id FROM track_assets WHERE track_id = ?`, r.TrackID).Scan(&asset)
	s.RecordPlay(ctx, PlayReport{Session: "skip", AssetID: asset, PositionMS: 3000, ListenedMS: 3000})
	s.RecordPlay(ctx, PlayReport{Session: "full", AssetID: asset, PositionMS: 60000, ListenedMS: 60000, Finished: true})
	s.RecordPlay(ctx, PlayReport{Session: "again", AssetID: asset, PositionMS: 40000, ListenedMS: 40000})
	h, err := s.History(ctx, 10, 0)
	if err != nil || len(h) != 2 || h[0].Album != "Al" {
		t.Fatalf("history = %+v %v", h, err)
	}
	top, err := s.TopTracks(ctx, 0, 10)
	if err != nil || len(top) != 1 || top[0].Plays != 2 {
		t.Fatalf("top = %+v %v", top, err)
	}
}
