package importer

import (
	"context"
	"path/filepath"
	"testing"
)

// Standalone tracks from an album folder without album tags (imported before the folder rule, or
// split off in the preview) are offered as an album of that folder; a folder with album tags is not.
func TestFolderGroups(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	startWorker(t, im)
	src := t.TempDir()
	taggedMP3(t, filepath.Join(src, "ARIA/Due COUR.1/Disc1/DUE10.mp3"), map[string]string{"TIT2": "DUE10"})
	taggedMP3(t, filepath.Join(src, "ARIA/Due COUR.1/Disc1/DUE9.mp3"), map[string]string{"TIT2": "DUE9"})
	taggedMP3(t, filepath.Join(src, "Set/Disc2/a.mp3"), map[string]string{"TIT2": "a", "TALB": "Real", "TPE2": "Z"})
	taggedMP3(t, filepath.Join(src, "Set/Disc1/b01.mp3"), map[string]string{"TIT2": "b"})
	taggedMP3(t, filepath.Join(src, "Box/x.mp3"), map[string]string{"TIT2": "x", "TALB": "X"})
	taggedMP3(t, filepath.Join(src, "Box/y.mp3"), map[string]string{"TIT2": "y", "TALB": "Y"})
	taggedMP3(t, filepath.Join(src, "Box/z.mp3"), map[string]string{"TIT2": "z"})
	batch, _, _ := im.CreateBatch(ctx, "local", "", src, true)
	waitState(t, im, batch, BatchReview)
	p, _ := im.Preview(ctx, batch)
	if err := im.ApplyOp(ctx, batch, PlanOp{Op: "standalone", Group: group(t, p, "Due COUR.1").Key}); err != nil {
		t.Fatal(err)
	}
	var b int64
	for _, it := range group(t, p, "Real").Items {
		if it.Plan.Title == "b" {
			b = it.ID
		}
	}
	if b == 0 { // the untagged disc of a set joins the tagged one
		t.Fatalf("b did not join Real: %+v", group(t, p, "Real"))
	}
	if err := im.ApplyOp(ctx, batch, PlanOp{Op: "move", Items: []int64{b}, Into: ""}); err != nil {
		t.Fatal(err)
	}
	if err := im.Start(ctx, batch); err != nil {
		t.Fatal(err)
	}
	waitState(t, im, batch, BatchDone)

	groups, err := im.FolderGroups(ctx)
	if err != nil || len(groups) != 2 { // not Box: its files went to several albums
		t.Fatalf("groups %+v %v", groups, err)
	}
	g, set := groups[0], groups[1]
	if set.Folder != "Set" || !set.Join || set.Title != "Real" || set.AlbumArtist != "Z" || len(set.Tracks) != 1 || set.Tracks[0].Track != 1 {
		t.Fatalf("set %+v", set)
	}
	if g.Folder != "ARIA/Due COUR.1" || g.Title != "Due COUR.1" || len(g.Tracks) != 2 || g.Tracks[0].File != "DUE9.mp3" ||
		g.Tracks[0].Track != 9 || g.Tracks[1].Track != 10 || g.Tracks[0].Disc != 1 || g.AlbumID != 0 {
		t.Fatalf("group %+v", g)
	}
	if n, err := im.MakeFolderAlbums(ctx, []FolderChoice{{Folder: "nowhere", Title: "x"}}); err != nil || n != 0 {
		t.Fatalf("unknown folder: %d %v", n, err)
	}
	gid, err := im.MakeFolderAlbums(ctx, []FolderChoice{{Folder: g.Folder, Title: "ARIA The STATION Due COUR.1", AlbumArtist: " "}})
	if err != nil || gid == 0 {
		t.Fatalf("make: %d %v", gid, err)
	}
	albums, _ := lib.Albums(ctx, 10, 0, false)
	var made int64
	for _, a := range albums {
		if a.Title == "ARIA The STATION Due COUR.1" && a.Tracks == 2 && a.AlbumArtist == "" {
			made = a.ID
		}
	}
	if made == 0 {
		t.Fatalf("albums %+v", albums)
	}
	if groups, _ := im.FolderGroups(ctx); len(groups) != 1 {
		t.Fatalf("offered: %+v", groups)
	}
	if _, err := im.MakeFolderAlbums(ctx, []FolderChoice{{Folder: "Set"}}); err != nil {
		t.Fatal(err)
	}
	real, _ := lib.Album(ctx, set.AlbumID)
	if len(real.Entries) != 2 || real.Entries[0].Title != "b" || real.Entries[0].DiscNo != 1 || real.Entries[0].Artist != "Z" {
		t.Fatalf("joined %+v", real.Entries)
	}
	if groups, _ := im.FolderGroups(ctx); len(groups) != 0 {
		t.Fatalf("still offered: %+v", groups)
	}
	// The album is the one the folder's files now import into.
	if a, _ := lib.AlbumByTags(ctx, "Due COUR.1", ""); a == nil || a.ID != made {
		t.Fatalf("album by tags %+v", a)
	}
}

func TestNaturalCompare(t *testing.T) {
	for _, c := range [][2]string{{"due9", "due10"}, {"DUE01", "due2"}, {"a", "b"}, {"track 2", "track 10"}, {"x1", "x1a"}} {
		if naturalCompare(c[0], c[1]) >= 0 || naturalCompare(c[1], c[0]) <= 0 {
			t.Errorf("%q should come before %q", c[0], c[1])
		}
	}
}

// Folders are told apart by where they were imported from: a folder of the same name in another
// import is another folder (#52), while the batches of one download share their folders.
func TestFolderGroupsBySource(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	startWorker(t, im)
	standalone := func(kind, root string, paths []string, album string) {
		t.Helper()
		for i := range paths {
			paths[i] = filepath.Join(root, paths[i])
		}
		batch, _, err := im.CreateBatchFiles(ctx, kind, "same name", root, paths, true)
		if err != nil {
			t.Fatal(err)
		}
		waitState(t, im, batch, BatchReview)
		p, _ := im.Preview(ctx, batch)
		if album != "" {
			if err := im.ApplyOp(ctx, batch, PlanOp{Op: "standalone", Group: group(t, p, album).Key}); err != nil {
				t.Fatal(err)
			}
		}
		if err := im.Start(ctx, batch); err != nil {
			t.Fatal(err)
		}
		waitState(t, im, batch, BatchDone)
	}
	a, b, dl := t.TempDir(), t.TempDir(), t.TempDir()
	taggedMP3(t, filepath.Join(a, "Release/01.mp3"), map[string]string{"TIT2": "one", "TALB": "Album A", "TPE2": "Artist A"})
	taggedMP3(t, filepath.Join(b, "Release/02.mp3"), map[string]string{"TIT2": "two", "TPE1": "Artist B"})
	taggedMP3(t, filepath.Join(dl, "Set/Disc2/a.mp3"), map[string]string{"TIT2": "a", "TALB": "Real", "TPE2": "Z"})
	taggedMP3(t, filepath.Join(dl, "Set/Disc1/b01.mp3"), map[string]string{"TIT2": "b"})
	standalone("local", a, []string{"Release/01.mp3"}, "")
	standalone("local", b, []string{"Release/02.mp3"}, "Release") // the folder rule made "Release": split it off
	standalone("download", dl, []string{"Set/Disc2/a.mp3"}, "")
	standalone("download", dl, []string{"Set/Disc1/b01.mp3"}, "Set") // the download's next batch

	groups, err := im.FolderGroups(ctx)
	if err != nil || len(groups) != 2 {
		t.Fatalf("groups %+v %v", groups, err)
	}
	rel, set := groups[0], groups[1]
	if rel.Folder != "Release" || rel.Join || rel.Title != "Release" || rel.AlbumArtist != "Artist B" || rel.Source.Kind != "local" ||
		len(rel.Tracks) != 1 || rel.Tracks[0].Title != "two" {
		t.Fatalf("the other import's folder: %+v", rel)
	}
	if set.Folder != "Set" || !set.Join || set.Title != "Real" || set.Source.Kind != "download" || len(set.Tracks) != 1 || rel.Key == set.Key {
		t.Fatalf("the download's next batch: %+v", set)
	}
	if _, err := im.MakeFolderAlbums(ctx, []FolderChoice{{Key: rel.Key, Title: "Album B", AlbumArtist: "Artist B"}}); err != nil {
		t.Fatal(err)
	}
	albums, _ := lib.Albums(ctx, 10, 0, false)
	got := map[string]int{}
	for _, al := range albums {
		got[al.Title+" / "+al.AlbumArtist] = al.Tracks
	}
	if got["Album A / Artist A"] != 1 || got["Album B / Artist B"] != 1 || got["Real / Z"] != 1 {
		t.Fatalf("albums %v", got)
	}
}
