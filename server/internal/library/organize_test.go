package library

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func albumOf(t *testing.T, s *Store, entry int64) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRow(`SELECT album_id FROM album_entries WHERE id = ?`, entry).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func searchCount(t *testing.T, s *Store, q string) (tracks, albums, artists int) {
	t.Helper()
	r, err := s.Search(context.Background(), q, 50)
	if err != nil {
		t.Fatal(err)
	}
	return len(r.Tracks), len(r.Albums), len(r.Artists)
}

func TestEditAndUndoTrack(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	r := publish(t, s, "e1", EntryInput{Title: "Untitled 01", Artist: "Unknown"})

	g, err := s.EditTrack(ctx, r.TrackID, TrackEdit{Title: Str("Euforia"), Artist: Str("Round Table"), Aliases: &[]string{"Euphoria", " ", "Euphoria"}})
	if err != nil || g == 0 {
		t.Fatalf("edit: %d %v", g, err)
	}
	if n, _ := s.EditTrack(ctx, r.TrackID, TrackEdit{Title: Str("Euforia")}); n != 0 {
		t.Fatal("an edit that changes nothing made a group")
	}
	if _, err := s.EditTrack(ctx, r.TrackID, TrackEdit{Title: Str("  ")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank title: %v", err)
	}
	d, _ := s.Track(ctx, r.TrackID)
	if d.Title != "Euforia" || d.Artist != "Round Table" || len(d.Aliases) != 1 {
		t.Fatalf("track = %+v", d)
	}
	if tr, _, ar := searchCount(t, s, "euphoria"); tr != 1 || ar != 0 {
		t.Fatalf("alias search: %d tracks", tr)
	}
	if _, _, ar := searchCount(t, s, "unknown"); ar != 0 {
		t.Fatal("the old artist, now unused, is still listed")
	}

	// A later correction of the title survives undoing the first edit.
	s.EditTrack(ctx, r.TrackID, TrackEdit{Title: Str("Euforia (TV size)")})
	undo, conflicts, err := s.Undo(ctx, g)
	if err != nil || undo == 0 {
		t.Fatalf("undo: %d %v", undo, err)
	}
	if len(conflicts) != 1 || conflicts[0].Field != "title" || conflicts[0].Reason != "changed" || conflicts[0].Name != "Euforia (TV size)" {
		t.Fatalf("conflicts = %+v", conflicts)
	}
	d, _ = s.Track(ctx, r.TrackID)
	if d.Title != "Euforia (TV size)" || d.Artist != "Unknown" || len(d.Aliases) != 0 {
		t.Fatalf("after undo: %+v", d)
	}
	if _, _, err := s.Undo(ctx, g); !errors.Is(err, ErrAlreadyUndone) {
		t.Fatalf("second undo: %v", err)
	}
	// Undoing the undo applies the change again.
	if _, _, err := s.Undo(ctx, undo); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.Track(ctx, r.TrackID); d.Artist != "Round Table" {
		t.Fatalf("redo: %+v", d)
	}
	groups, _ := s.EditGroups(ctx, 10, 0)
	if len(groups) != 4 || groups[1].Source != SourceUndo || groups[1].UndoOf != g {
		t.Fatalf("groups = %+v", groups)
	}
	eg, _ := s.EditGroup(ctx, g)
	if eg.UndoneBy != undo || len(eg.Edits) != 3 || eg.Edits[0].Name == "" {
		t.Fatalf("group = %+v", eg)
	}
}

func TestEditAlbumAndReimport(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	in := EntryInput{Title: "Track 1", Artist: "A", Album: "アルバム", AlbumArtist: "A", TrackNo: 1}
	r1 := publish(t, s, "a1", in)
	r2 := publish(t, s, "a2", EntryInput{Title: "Track 2", Artist: "A", Album: "アルバム", AlbumArtist: "A", TrackNo: 2})
	album := albumOf(t, s, r1.EntryID)

	_, err := s.EditAlbum(ctx, album, AlbumEdit{Title: Str("Album"), Catalog: Str("ABCD-1234"), Kind: Str("spoken"),
		Entries: []EntryEdit{{EntryID: r2.EntryID, TrackNo: ptr(5), TrackEdit: TrackEdit{Title: Str("Second")}}}})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.Album(ctx, album)
	if d.Title != "Album" || d.Catalog != "ABCD-1234" || d.Entries[1].TrackNo != 5 || d.Entries[1].Title != "Second" || d.Entries[0].Kind != "spoken" {
		t.Fatalf("album = %+v", d)
	}
	if _, err := s.EditAlbum(ctx, album, AlbumEdit{Entries: []EntryEdit{{EntryID: 999}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign entry: %v", err)
	}
	if _, al, _ := searchCount(t, s, "あるばむ"); al != 0 {
		t.Fatal("the old title is still indexed")
	}

	// Importing the same file again, with its old tags, is the same entry: no ghost album.
	again := publish(t, s, "a1", in)
	if again.Created || again.EntryID != r1.EntryID {
		t.Fatalf("re-import = %+v", again)
	}
	// A new file with the old tags joins the renamed album.
	r3 := publish(t, s, "a3", EntryInput{Title: "Track 3", Album: "アルバム", AlbumArtist: "A", TrackNo: 3})
	if albumOf(t, s, r3.EntryID) != album {
		t.Fatal("new file with the original tags made a new album")
	}
	if list, _ := s.Albums(ctx, 10, 0, false); len(list) != 1 {
		t.Fatalf("albums = %+v", list)
	}
}

func ptr(n int) *int { return &n }

func TestMergeSplitRemove(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a1 := publish(t, s, "m1", EntryInput{Title: "One", Album: "Disc A", AlbumArtist: "X", TrackNo: 1})
	a2 := publish(t, s, "m2", EntryInput{Title: "Two", Album: "Disc A", AlbumArtist: "X", TrackNo: 2})
	b1 := publish(t, s, "m3", EntryInput{Title: "Three", Album: "Disc B", AlbumArtist: "X", TrackNo: 1})
	dup := publish(t, s, "m1", EntryInput{Title: "One", Album: "Disc B", AlbumArtist: "X", TrackNo: 9}) // same file on B
	from, into := albumOf(t, s, a1.EntryID), albumOf(t, s, b1.EntryID)
	s.SetFavorite(ctx, "album", from, true)

	g, err := s.MergeAlbum(ctx, from, into)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.Album(ctx, into)
	if len(d.Entries) != 3 { // One is already on B: not added twice
		t.Fatalf("merged entries = %d", len(d.Entries))
	}
	if list, _ := s.Albums(ctx, 10, 0, false); len(list) != 1 {
		t.Fatalf("emptied album still listed: %+v", list)
	}
	if f, _ := s.Favorites(ctx); len(f.Albums) != 1 || f.Albums[0].ID != into {
		t.Fatalf("favorite did not follow the merge: %+v", f.Albums)
	}
	if src, _ := s.Album(ctx, from); src.MergedInto != into || len(src.Entries) != 0 {
		t.Fatalf("source = %+v", src)
	}
	// Later imports tagged as Disc A follow the merge.
	a4 := publish(t, s, "m4", EntryInput{Title: "Four", Album: "Disc A", AlbumArtist: "X", TrackNo: 4})
	if albumOf(t, s, a4.EntryID) != into {
		t.Fatal("import ignored the merge")
	}
	if _, err := s.MergeAlbum(ctx, into, from); !errors.Is(err, ErrInvalid) {
		t.Fatalf("merge into an emptied album: %v", err)
	}

	// Undo puts both entries back, including the one removed as a duplicate.
	if _, c, err := s.Undo(ctx, g); err != nil || len(c) != 0 {
		t.Fatalf("undo merge: %+v %v", c, err)
	}
	if src, _ := s.Album(ctx, from); len(src.Entries) != 2 || src.MergedInto != 0 {
		t.Fatalf("after undo: %+v", src)
	}
	_ = dup

	// Split one entry off into a new album, then undo it.
	newID, sg, err := s.SplitAlbum(ctx, from, []int64{a2.EntryID}, "Disc A (bonus)")
	if err != nil || newID == 0 || albumOf(t, s, a2.EntryID) != newID {
		t.Fatalf("split: %d %v", newID, err)
	}
	if nd, _ := s.Album(ctx, newID); nd.Original || nd.AlbumArtist != "X" {
		t.Fatalf("new album = %+v", nd)
	}
	if _, _, err := s.SplitAlbum(ctx, from, []int64{b1.EntryID}, "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("split of a foreign entry: %v", err)
	}
	s.Undo(ctx, sg)
	if albumOf(t, s, a2.EntryID) != from {
		t.Fatal("undo split")
	}

	// Removing the album keeps its tracks as standalone songs; undo brings the entries back.
	rg, err := s.RemoveEntries(ctx, from, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tr, _ := s.Track(ctx, a2.TrackID); tr == nil || len(tr.Entries) != 0 {
		t.Fatalf("track after removal = %+v", tr)
	}
	if _, c, err := s.Undo(ctx, rg); err != nil || len(c) != 0 {
		t.Fatalf("undo removal: %+v %v", c, err)
	}
	if d, _ := s.Album(ctx, from); len(d.Entries) != 2 {
		t.Fatalf("entries after undo = %d", len(d.Entries))
	}
}

func TestRestoreAlbum(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	orig := map[int64]*EntryInput{}
	add := func(sha string, in EntryInput) PublishResult {
		r := publish(t, s, sha, in)
		c := in
		orig[r.TrackID] = &c
		return r
	}
	a := add("r1", EntryInput{Title: "Song", Artist: "S", Album: "Live", AlbumArtist: "S", Date: "2004", TrackNo: 1})
	b := add("r2", EntryInput{Title: "Other", Artist: "S", Album: "Live 2", AlbumArtist: "S", TrackNo: 1})
	album, other := albumOf(t, s, a.EntryID), albumOf(t, s, b.EntryID)
	original := func(_ context.Context, id int64) (*EntryInput, error) { return orig[id], nil }

	s.EditAlbum(ctx, album, AlbumEdit{Title: Str("Live!"), Date: Str("2005"),
		Entries: []EntryEdit{{EntryID: a.EntryID, TrackNo: ptr(7), TrackEdit: TrackEdit{Title: Str("Song (live)"), Kind: Str("spoken")}}}})
	s.MergeAlbum(ctx, other, album)

	g, err := s.RestoreAlbum(ctx, album, original)
	if err != nil || g == 0 {
		t.Fatalf("restore: %d %v", g, err)
	}
	d, _ := s.Album(ctx, album)
	if d.Title != "Live" || d.Date != "2004" || len(d.Entries) != 1 || d.Entries[0].TrackNo != 1 || d.Entries[0].Title != "Song" || d.Entries[0].Kind != "music" {
		t.Fatalf("restored = %+v", d)
	}
	if o, _ := s.Album(ctx, other); len(o.Entries) != 1 || o.MergedInto != 0 {
		t.Fatalf("merged album not split back: %+v", o)
	}
	if eg, _ := s.EditGroup(ctx, g); eg.Source != SourceRestore {
		t.Fatalf("group = %+v", eg)
	}
	if n, _ := s.RestoreAlbum(ctx, album, original); n != 0 {
		t.Fatal("restoring an unchanged album changed something")
	}
}

func TestRenameArtist(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	publish(t, s, "n1", EntryInput{Title: "A", Artist: "Mizuki Nana", Album: "Al", AlbumArtist: "Mizuki Nana"})
	publish(t, s, "n2", EntryInput{Title: "B", Artist: "Mizuki Nana feat. X"})
	old, _ := s.ArtistByName(ctx, "Mizuki Nana")
	s.SetAliases(ctx, "artist", old, []string{"NANA"})
	if _, err := s.RenameArtist(ctx, old, "水樹奈々"); err != nil {
		t.Fatal(err)
	}
	now, _ := s.ArtistByName(ctx, "水樹奈々")
	d, _ := s.ArtistDetail(ctx, now)
	if d == nil || len(d.Items) != 1 {
		t.Fatalf("renamed artist = %+v", d)
	}
	if list, _ := s.Albums(ctx, 10, 0, false); list[0].AlbumArtist != "水樹奈々" {
		t.Fatalf("album artist = %q", list[0].AlbumArtist)
	}
	if list, _ := s.Artists(ctx, 10, 0); len(list) != 2 { // the old name is no longer listed
		t.Fatalf("artists = %+v", list)
	}
	if len(d.Aliases) != 2 || d.Aliases[0] != "Mizuki Nana" || d.Aliases[1] != "NANA" {
		t.Fatalf("aliases did not follow the rename: %v", d.Aliases)
	}
	r, _ := s.Search(ctx, "mizuki nana", 50)
	if !slices.ContainsFunc(r.Artists, func(a Artist) bool { return a.ID == now }) {
		t.Fatalf("the old name no longer finds the artist: %+v", r.Artists)
	}
}

func TestDeleteTrackKeepsSharedFiles(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	only := publish(t, s, "d1", EntryInput{Title: "Only", Album: "Al", AlbumArtist: "A", TrackNo: 1})
	s.SetFavorite(ctx, "track", only.TrackID, true)
	s.SetLyrics(ctx, only.TrackID, LyricsManual, "words")
	files, err := s.DeleteTrack(ctx, only.TrackID)
	if err != nil || len(files) != 1 || files[0] != "drive-d1" {
		t.Fatalf("delete: %v %v", files, err)
	}
	if tr, _ := s.Track(ctx, only.TrackID); tr != nil {
		t.Fatal("track still there")
	}
	if a, _ := s.AssetByHash(ctx, "d1", 1000); a != nil {
		t.Fatal("unused asset kept")
	}
	// A second track sharing the file keeps it.
	shared := publish(t, s, "d2", EntryInput{Title: "Shared"})
	var asset int64
	s.db.QueryRow(`SELECT asset_id FROM track_assets WHERE track_id = ?`, shared.TrackID).Scan(&asset)
	s.db.Exec(`INSERT INTO tracks (title, created_at, updated_at) VALUES ('Twin', 0, 0)`)
	s.db.Exec(`INSERT INTO track_assets (track_id, asset_id) SELECT max(id), ? FROM tracks`, asset)
	if files, err := s.DeleteTrack(ctx, shared.TrackID); err != nil || len(files) != 0 {
		t.Fatalf("shared file was released: %v %v", files, err)
	}
}

func TestMissingTrackCanBeEditedAndDeleted(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	kept := publish(t, s, "m1", EntryInput{Title: "Kept", Album: "Gone", AlbumArtist: "A", TrackNo: 1})
	lost := publish(t, s, "m2", EntryInput{Title: "Lost", Album: "Gone", AlbumArtist: "A", TrackNo: 2})
	if n, missing, err := s.ObserveDriveFile(ctx, DriveObservation{ID: "drive-m2"}); n != 1 || !missing || err != nil {
		t.Fatalf("mark: %d %v %v", n, missing, err)
	}
	tr, err := s.Track(ctx, lost.TrackID)
	if err != nil || tr == nil || !tr.Missing {
		t.Fatalf("missing track: %+v %v", tr, err)
	}
	if tr, _ := s.Track(ctx, kept.TrackID); tr == nil || tr.Missing {
		t.Fatalf("kept track: %+v", tr)
	}
	album := albumOf(t, s, lost.EntryID)
	if d, _ := s.Album(ctx, album); len(d.Entries) != 1 {
		t.Fatalf("album page shows %d entries", len(d.Entries))
	}
	if ids, err := s.AlbumTrackIDs(ctx, album); err != nil || len(ids) != 2 {
		t.Fatalf("album tracks %v %v", ids, err)
	}
	if files, err := s.DeleteTrack(ctx, lost.TrackID); err != nil || len(files) != 1 {
		t.Fatalf("delete: %v %v", files, err)
	}
	if m, _ := s.Missing(ctx); len(m) != 0 {
		t.Fatalf("still listed as missing: %+v", m)
	}
}

// A merged album's CUE sheets and logs show on the album it went into, once each, and go back with
// an undo (review #27).
func TestMergeKeepsSidecars(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a := publish(t, s, "sa", EntryInput{Title: "One", Album: "Source", AlbumArtist: "X", TrackNo: 1})
	b := publish(t, s, "sb", EntryInput{Title: "Two", Album: "Target", AlbumArtist: "X", TrackNo: 2})
	src, dst := albumOf(t, s, a.EntryID), albumOf(t, s, b.EntryID)
	s.AddSidecar(ctx, src, "disc.cue", "cue", "c1", 10, "drive-cue")
	s.AddSidecar(ctx, src, "rip.log", "log", "l1", 20, "drive-log")
	s.AddSidecar(ctx, dst, "rip.log", "log", "l1", 20, "drive-log2") // the same log kept on both
	g, err := s.MergeAlbum(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.Album(ctx, dst)
	if len(d.Entries) != 2 || len(d.Sidecars) != 2 {
		t.Fatalf("after merge: %d entries, sidecars %+v", len(d.Entries), d.Sidecars)
	}
	if c, _ := s.FindSidecar(ctx, dst, "c1", 10); c == nil {
		t.Fatal("a re-import would keep the merged album's CUE again")
	}
	if _, _, err := s.Undo(ctx, g); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Album(ctx, dst); len(d.Sidecars) != 1 {
		t.Fatalf("after undo the target has %+v", d.Sidecars)
	}
	if d, _ := s.Album(ctx, src); len(d.Sidecars) != 2 {
		t.Fatalf("after undo the source has %+v", d.Sidecars)
	}
}

// A field changed after the change being undone stays, even when it was changed back to the same
// value (A→B→C→B); a change that was itself undone does not count (review #20).
func TestUndoKeepsLaterReturnToSameValue(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	r := publish(t, s, "u1", EntryInput{Title: "A"})
	title := func() string { tr, _ := s.Track(ctx, r.TrackID); return tr.Title }
	set := func(v string) int64 {
		g, err := s.EditTrack(ctx, r.TrackID, TrackEdit{Title: &v})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	g1 := set("B")
	set("C")
	set("B")
	if _, conflicts, err := s.Undo(ctx, g1); err != nil || len(conflicts) != 1 || title() != "B" {
		t.Fatalf("undo: %v %+v, title %q", err, conflicts, title())
	}
	// A later change that was undone cancels out: undoing the first one then works.
	r2 := publish(t, s, "u2", EntryInput{Title: "X"})
	set2 := func(v string) int64 { g, _ := s.EditTrack(ctx, r2.TrackID, TrackEdit{Title: &v}); return g }
	h1 := set2("Y")
	h2 := set2("Z")
	if _, _, err := s.Undo(ctx, h2); err != nil {
		t.Fatal(err)
	}
	if _, conflicts, err := s.Undo(ctx, h1); err != nil || len(conflicts) != 0 {
		t.Fatalf("undo after the later change was undone: %v %+v", err, conflicts)
	}
	if tr, _ := s.Track(ctx, r2.TrackID); tr.Title != "X" {
		t.Fatalf("title %q", tr.Title)
	}
}

// A new album the user chose gets its own entry for a file imported before, sharing the file;
// re-importing without that choice still finds the earlier entry (review #21).
func TestChosenNewAlbumKeepsIndependentEntry(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	first := publish(t, s, "n1", EntryInput{Title: "Song", Album: "Original", AlbumArtist: "X", TrackNo: 1})
	var asset int64
	s.db.QueryRow(`SELECT asset_id FROM album_entries WHERE id = ?`, first.EntryID).Scan(&asset)
	again, err := s.Publish(ctx, asset, EntryInput{Title: "Song", Album: "Original", AlbumArtist: "X", TrackNo: 1})
	if err != nil || again.EntryID != first.EntryID || again.Created {
		t.Fatalf("plain re-import: %+v %v", again, err)
	}
	other, err := s.Publish(ctx, asset, EntryInput{Title: "Song", Album: "Another Edition", AlbumArtist: "X", TrackNo: 1,
		NewAlbum: true, Chosen: true, Tagged: &Tagged{Album: "Original", AlbumArtist: "X", Track: 1}})
	if err != nil || other.EntryID == first.EntryID || !other.Created {
		t.Fatalf("chosen new album: %+v %v", other, err)
	}
	if a, b := albumOf(t, s, first.EntryID), albumOf(t, s, other.EntryID); a == b {
		t.Fatal("both entries on one album")
	}
}
