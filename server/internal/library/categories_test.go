package library

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Categories hold albums the user puts in them (review #92): an album in several, none twice; a
// batch is one edit that undo takes back, a category made in the same edit too; a merge passes the
// emptied albums' categories on and undo takes them back; deleting a category leaves its albums and
// undo brings it back with them.
func TestCategories(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := fixture{t, s}
	A := f.albumOf(f.song("a", "A1", "A", 1, 1).EntryID)
	B := f.albumOf(f.song("b", "B1", "B", 1, 1).EntryID)
	C := f.albumOf(f.song("c", "C1", "C", 1, 1).EntryID)
	aria, err := s.CreateCategory(ctx, "  ARIA ")
	if err != nil || aria.Name != "ARIA" {
		t.Fatalf("create: %+v %v", aria, err)
	}
	umi, _ := s.CreateCategory(ctx, "海貓")
	if _, err := s.CreateCategory(ctx, "aria"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("same name: %v", err)
	}
	if _, err := s.CreateCategory(ctx, " "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no name: %v", err)
	}
	counts := func(albums ...int64) string {
		t.Helper()
		l, err := s.Categories(ctx, albums)
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, c := range l.Categories {
			out += fmt.Sprintf("%s:%d/%d ", c.Name, c.Albums, c.Selected)
		}
		return out + fmt.Sprintf("none:%d", l.Uncategorized)
	}
	in := func(cat int64) string {
		t.Helper()
		list, err := s.AlbumsBy(ctx, AlbumQuery{Limit: 50, Category: cat})
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, a := range list {
			out += a.Title
		}
		return out
	}

	g, _, err := s.Categorize(ctx, CategorizeRequest{Albums: []int64{A, B}, Add: []int64{aria.ID}})
	if err != nil || g == 0 {
		t.Fatal(g, err)
	}
	if _, _, err := s.Categorize(ctx, CategorizeRequest{Albums: []int64{A}, Add: []int64{umi.ID}}); err != nil {
		t.Fatal(err)
	}
	if got := counts(A, C); got != "ARIA:2/1 海貓:1/1 none:1" {
		t.Fatalf("counts %s", got)
	}
	if in(aria.ID) != "AB" || in(umi.ID) != "A" || in(-1) != "C" {
		t.Fatalf("in ARIA %q, 海貓 %q, none %q", in(aria.ID), in(umi.ID), in(-1))
	}
	if g, _, err := s.Categorize(ctx, CategorizeRequest{Albums: []int64{A, B}, Add: []int64{aria.ID}}); err != nil || g != 0 {
		t.Fatalf("added again: %d %v", g, err)
	}
	if _, _, err := s.Categorize(ctx, CategorizeRequest{Albums: []int64{A, 999}, Add: []int64{aria.ID}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown album: %v", err)
	}
	if _, _, err := s.Categorize(ctx, CategorizeRequest{Albums: []int64{A}, Add: []int64{12345}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown category: %v", err)
	}
	if eg, _ := s.EditGroup(ctx, g); len(eg.Edits) != 2 || eg.Edits[0].NewLabel != "ARIA" || eg.Edits[0].OldLabel != "（無）" {
		t.Fatalf("edit log %+v", eg.Edits)
	}

	// A new category made in the same edit; undo takes both back, redo brings both.
	g2, made, err := s.Categorize(ctx, CategorizeRequest{Albums: []int64{C}, Create: "P3R"})
	if err != nil || made == nil || in(made.ID) != "C" {
		t.Fatalf("made with the albums: %+v %v", made, err)
	}
	undo, conflicts, err := s.Undo(ctx, g2)
	if err != nil || len(conflicts) != 0 || counts() != "ARIA:2/0 海貓:1/0 none:1" {
		t.Fatalf("undo: %v %+v %s", err, conflicts, counts())
	}
	if _, conflicts, err := s.Undo(ctx, undo); err != nil || len(conflicts) != 0 || counts() != "ARIA:2/0 P3R:1/0 海貓:1/0 none:0" {
		t.Fatalf("redo: %v %+v %s", err, conflicts, counts())
	}

	// A merged into B: B is in A's categories besides its own, A in none; undo puts them back.
	_, mg, err := s.MergeAlbums(ctx, MergeRequest{Albums: []int64{A, B}, Into: B})
	if err != nil {
		t.Fatal(err)
	}
	if in(aria.ID) != "B" || in(umi.ID) != "B" {
		t.Fatalf("after merge: ARIA %q 海貓 %q", in(aria.ID), in(umi.ID))
	}
	if _, conflicts, err := s.Undo(ctx, mg); err != nil || len(conflicts) != 0 || in(aria.ID) != "AB" || in(umi.ID) != "A" {
		t.Fatalf("undo merge: %v %+v ARIA %q 海貓 %q", err, conflicts, in(aria.ID), in(umi.ID))
	}

	// Deleting a category leaves its albums; undo brings it back with them.
	dg, err := s.DeleteCategory(ctx, aria.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := counts(); got != "P3R:1/0 海貓:1/0 none:1" {
		t.Fatalf("after delete: %s", got)
	}
	if list, _ := s.Albums(ctx, 50, 0, false); len(list) != 3 {
		t.Fatalf("albums after delete: %d", len(list))
	}
	if _, conflicts, err := s.Undo(ctx, dg); err != nil || len(conflicts) != 0 || in(aria.ID) != "AB" {
		t.Fatalf("undo delete: %v %+v %q", err, conflicts, in(aria.ID))
	}
	if d, _ := s.Album(ctx, A); len(d.Categories) != 2 || d.Categories[0].Name != "ARIA" {
		t.Fatalf("album's categories %+v", d.Categories)
	}
	if err := s.RenameCategory(ctx, umi.ID, "うみねこ"); err != nil || in(umi.ID) != "A" {
		t.Fatalf("rename: %v", err)
	}
	if err := s.RenameCategory(ctx, umi.ID, "ARIA"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("rename to a taken name: %v", err)
	}
}

// A category holding more albums than a page is listed page by page, searched and ordered.
func TestCategoryPages(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	f := fixture{t, s}
	var ids []int64
	for i := 0; i < 205; i++ {
		ids = append(ids, f.albumOf(f.song(fmt.Sprint("s", i), "T", fmt.Sprintf("Album %03d", i), 1, 1).EntryID))
	}
	f.song("other", "T", "Other", 1, 1)
	_, made, err := s.Categorize(ctx, CategorizeRequest{Albums: ids, Create: "Big"})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := s.AlbumsBy(ctx, AlbumQuery{Limit: 200, Category: made.ID})
	rest, _ := s.AlbumsBy(ctx, AlbumQuery{Limit: 200, Offset: 200, Category: made.ID})
	if len(first) != 200 || len(rest) != 5 || rest[4].Title != "Album 204" {
		t.Fatalf("pages %d %d", len(first), len(rest))
	}
	if found, _ := s.AlbumsBy(ctx, AlbumQuery{Limit: 200, Category: made.ID, Search: "album 1_"}); len(found) != 0 {
		t.Fatalf("_ is not a wildcard: %d", len(found))
	}
	if found, _ := s.AlbumsBy(ctx, AlbumQuery{Limit: 200, Category: made.ID, Search: "Album 19"}); len(found) != 10 {
		t.Fatalf("search: %d", len(found))
	}
	if recent, _ := s.AlbumsBy(ctx, AlbumQuery{Limit: 1, Category: made.ID, Recent: true}); recent[0].Title != "Album 204" {
		t.Fatalf("recent first: %s", recent[0].Title)
	}
	if none, _ := s.AlbumsBy(ctx, AlbumQuery{Limit: 200, Category: -1}); len(none) != 1 || none[0].Title != "Other" {
		t.Fatalf("in no category: %+v", none)
	}
}
