package library

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Works (review #94): kept as the source says them; albums linked by hand, several to an album and
// several albums to a work; a link corrected carries the songs' uses along; unlinking takes the
// album's songs' uses away; every one an edit that undo takes back; a merge passes them on; the
// lists show only works something is linked to.
func TestWorks(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := fixture{t, s}
	a1 := f.song("w1", "Undine", "ARIA The ANIMATION OST", 1, 1)
	a2 := f.song("w2", "Rainbow", "ARIA The ANIMATION OST", 1, 2)
	A := f.albumOf(a1.EntryID)
	B := f.albumOf(f.song("w3", "Euforia", "ARIA The NATURAL OST", 1, 1).EntryID)
	var t1, t2 int64
	s.db.QueryRow(`SELECT track_id FROM album_entries WHERE id = ?`, a1.EntryID).Scan(&t1)
	s.db.QueryRow(`SELECT track_id FROM album_entries WHERE id = ?`, a2.EntryID).Scan(&t2)

	put := func(sid, name, cn string, score float64) int64 {
		t.Helper()
		id, err := s.PutWork(ctx, WorkData{Source: SourceBangumi, SourceID: sid, Type: 2, Name: name, NameCN: cn, Score: score, Votes: 10,
			Image: "https://lain.bgm.tv/pic/" + sid + ".jpg"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	anim := put("531", "ARIA The ANIMATION", "水星领航员", 8.2)
	if again := put("531", "ARIA The ANIMATION", "水星领航员", 8.3); again != anim {
		t.Fatalf("the same subject is the same work: %d %d", again, anim)
	}
	natural := put("1269", "ARIA The NATURAL", "", 0)
	put("750", "ARIA The OVA", "", 0) // read, never linked
	if _, err := s.PutWork(ctx, WorkData{Source: SourceBangumi, SourceID: "1"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no name: %v", err)
	}
	if w, _ := s.WorkFor(ctx, SourceBangumi, "1269"); w == nil || w.Score != 0 || w.Votes != 10 || w.NameCN != "" {
		t.Fatalf("what the source does not say stays unsaid: %+v", w)
	}
	listed := func() string {
		t.Helper()
		list, err := s.Works(ctx, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, w := range list {
			out = append(out, fmt.Sprintf("%s:%d/%d", w.Name, w.Albums, w.Tracks))
		}
		return strings.Join(out, " ")
	}
	worksOf := func(album int64) string {
		t.Helper()
		d, err := s.Album(ctx, album)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, w := range d.Works {
			out = append(out, w.Name)
		}
		for _, e := range d.Entries {
			for _, u := range e.Works {
				out = append(out, fmt.Sprintf("%s=%d:%s:%s", e.Title, u.Work, u.Use, u.Note))
			}
		}
		return strings.Join(out, ",")
	}
	if listed() != "" {
		t.Fatalf("nothing linked, nothing listed: %s", listed())
	}

	g, err := s.LinkWork(ctx, A, anim, 0)
	if err != nil || g == 0 {
		t.Fatal(g, err)
	}
	if g, err := s.LinkWork(ctx, A, anim, 0); err != nil || g != 0 {
		t.Fatalf("linked again: %d %v", g, err)
	}
	if _, err := s.LinkWork(ctx, A, natural, 0); err != nil { // an album of two works
		t.Fatal(err)
	}
	if _, err := s.LinkWork(ctx, B, natural, 0); err != nil { // a work of two albums
		t.Fatal(err)
	}
	if _, err := s.LinkWork(ctx, A, 999, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown work: %v", err)
	}
	if _, err := s.LinkWork(ctx, B, anim, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replacing an unknown work: %v", err)
	}
	if got := listed(); got != "ARIA The ANIMATION:1/0 ARIA The NATURAL:2/0" {
		t.Fatalf("listed %s", got)
	}

	if _, err := s.SetTrackWorks(ctx, t1, []TrackWork{{Work: anim, Use: "op", Note: "  第 1–13 話 "}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTrackWorks(ctx, t2, []TrackWork{{Work: anim, Use: "ed"}, {Work: natural, Use: "insert"}}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]TrackWork{{{Work: anim, Use: "opening"}}, {{Work: anim}, {Work: anim}}, {{Work: anim, Note: strings.Repeat("長", 201)}}} {
		if _, err := s.SetTrackWorks(ctx, t1, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	if _, err := s.SetTrackWorks(ctx, t1, []TrackWork{{Work: 999}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown work: %v", err)
	}
	want := fmt.Sprintf("ARIA The ANIMATION,ARIA The NATURAL,Undine=%d:op:第 1–13 話,Rainbow=%d:ed:,Rainbow=%d:insert:", anim, anim, natural)
	if got := worksOf(A); got != want {
		t.Fatalf("album:\n%s\nwant\n%s", got, want)
	}
	d, err := s.Work(ctx, anim)
	if err != nil || len(d.Albums) != 1 || d.Albums[0].ID != A || len(d.Songs) != 2 || d.Songs[0].Title != "Undine" || d.Songs[0].Use != "op" ||
		d.Songs[1].Use != "ed" || d.Work.NameCN != "水星领航员" || !d.Work.Image || d.Work.Score != 8.3 {
		t.Fatalf("work page: %+v %v", d, err)
	}

	// A link corrected: the songs' uses go with it, and undo puts both back.
	other := put("1270", "ARIA The ORIGINATION", "", 0)
	g, err = s.LinkWork(ctx, A, other, anim)
	if err != nil || g == 0 {
		t.Fatal(g, err)
	}
	want2 := fmt.Sprintf("ARIA The NATURAL,ARIA The ORIGINATION,Undine=%d:op:第 1–13 話,Rainbow=%d:insert:,Rainbow=%d:ed:", other, natural, other)
	if got := worksOf(A); got != want2 {
		t.Fatalf("replaced:\n%s\nwant\n%s", got, want2)
	}
	eg, _ := s.EditGroup(ctx, g)
	if !strings.Contains(eg.Summary, "從「ARIA The ANIMATION」改為「ARIA The ORIGINATION」") {
		t.Fatalf("summary %q", eg.Summary)
	}
	labels := ""
	for _, e := range eg.Edits {
		labels += e.Target + ":" + e.OldLabel + "→" + e.NewLabel + ";"
	}
	if !strings.Contains(labels, "track:ARIA The ANIMATION（片頭曲） 第 1–13 話→ARIA The ORIGINATION（片頭曲） 第 1–13 話;") ||
		!strings.Contains(labels, "album:ARIA The ANIMATION、ARIA The NATURAL→ARIA The NATURAL、ARIA The ORIGINATION;") {
		t.Fatalf("labels %s", labels)
	}
	if _, conflicts, err := s.Undo(ctx, g); err != nil || len(conflicts) > 0 {
		t.Fatal(conflicts, err)
	}
	if got := worksOf(A); got != want {
		t.Fatalf("undone:\n%s\nwant\n%s", got, want)
	}

	// Unlinked: the album's songs' uses of it go too; undo brings all back.
	g, err = s.UnlinkWork(ctx, A, anim)
	if err != nil || g == 0 {
		t.Fatal(g, err)
	}
	if got, want := worksOf(A), fmt.Sprintf("ARIA The NATURAL,Rainbow=%d:insert:", natural); got != want {
		t.Fatalf("unlinked: %s", got)
	}
	if got := listed(); got != "ARIA The NATURAL:2/1" {
		t.Fatalf("listed after unlinking %s", got)
	}
	if g, err := s.UnlinkWork(ctx, A, anim); err != nil || g != 0 {
		t.Fatalf("unlinked again: %d %v", g, err)
	}
	if _, _, err := s.Undo(ctx, g); err != nil {
		t.Fatal(err)
	}
	if got := worksOf(A); got != want {
		t.Fatalf("unlink undone:\n%s\nwant\n%s", got, want)
	}

	// A merge passes the links on.
	g, err = s.MergeAlbum(ctx, B, A)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Album(ctx, B); len(d.Works) != 0 {
		t.Fatalf("merged away keeps %+v", d.Works)
	}
	if got := listed(); got != "ARIA The ANIMATION:1/2 ARIA The NATURAL:1/1" {
		t.Fatalf("listed after the merge %s", got)
	}
	if _, _, err := s.Undo(ctx, g); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.Album(ctx, B); len(d.Works) != 1 || d.Works[0].ID != natural {
		t.Fatalf("merge undone: %+v", d.Works)
	}

	res, err := s.Search(ctx, "领航", 10)
	if err != nil || len(res.Works) != 1 || res.Works[0].ID != anim {
		t.Fatalf("search: %+v %v", res, err)
	}
	if res, _ := s.Search(ctx, "OVA", 10); len(res.Works) != 0 {
		t.Fatalf("a work nothing is linked to is not found: %+v", res.Works)
	}
	if list, _ := s.Works(ctx, 4, false); len(list) != 0 {
		t.Fatalf("no games: %+v", list)
	}
}
