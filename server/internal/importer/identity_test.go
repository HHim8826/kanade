package importer

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// albumsNow lists the library's albums as "title | album artist | tracks".
func albumsNow(t *testing.T, im *Importer) []string {
	t.Helper()
	albums, err := im.lib.Albums(context.Background(), 50, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, a := range albums {
		out = append(out, fmt.Sprintf("%s | %s | %d", a.Title, a.AlbumArtist, a.Tracks))
	}
	sort.Strings(out)
	return out
}

// A download's rounds are imported apart; the album a folder's songs make does not depend on which
// round brought which song (review #81).
func TestRoundsMakeOneAlbum(t *testing.T) {
	type song struct{ name, artist, albumArtist string }
	for _, tc := range []struct {
		name   string
		album  string // "" leaves the album tag out: the folder names it
		songs  []song
		rounds [][]int
		want   string
	}{
		{"no album artist", "Collection", []song{{"1", "A", ""}, {"2", "B", ""}, {"3", "B", ""}, {"4", "C", ""}},
			[][]int{{0}, {1}, {2, 3}}, "Collection | Various Artists | 4"},
		{"no album tag", "", []song{{"1", "A", ""}, {"2", "B", ""}, {"3", "B", ""}, {"4", "C", ""}},
			[][]int{{0}, {1}, {2, 3}}, "Collection | Various Artists | 4"},
		{"album artist on the first round", "Collection", []song{{"1", "A", "Ensemble"}, {"2", "B", ""}, {"3", "B", ""}, {"4", "C", ""}},
			[][]int{{0}, {1}, {2, 3}}, "Collection | Ensemble | 4"},
		{"album artist on a later round", "Collection", []song{{"1", "A", ""}, {"2", "B", "Ensemble"}, {"3", "B", ""}, {"4", "C", ""}},
			[][]int{{0}, {1}, {2, 3}}, "Collection | Ensemble | 4"},
		{"one artist", "Collection", []song{{"1", "A", ""}, {"2", "A", ""}},
			[][]int{{0}, {1}}, "Collection | A | 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			for _, split := range []bool{false, true} {
				im, _, _ := setup(t)
				root := t.TempDir()
				var paths []string
				for i, s := range tc.songs {
					frames := map[string]string{"TIT2": "Song " + s.name, "TPE1": s.artist, "TRCK": fmt.Sprint(i + 1)}
					if tc.album != "" {
						frames["TALB"] = tc.album
					}
					if s.albumArtist != "" {
						frames["TPE2"] = s.albumArtist
					}
					p := filepath.Join(root, "Collection", fmt.Sprintf("%02d.mp3", i+1))
					taggedMP3(t, p, frames)
					paths = append(paths, p)
				}
				rounds := [][]int{{}}
				for i := range tc.songs {
					rounds[0] = append(rounds[0], i)
				}
				if split {
					rounds = tc.rounds
				}
				for _, r := range rounds {
					var these []string
					for _, i := range r {
						these = append(these, paths[i])
					}
					b, _, err := im.CreateBatchFiles(ctx, "download", "Box", root, these, false)
					if err != nil {
						t.Fatal(err)
					}
					runUntilDone(t, im, b)
				}
				if got := albumsNow(t, im); len(got) != 1 || got[0] != tc.want {
					t.Fatalf("in rounds %v: %v, want %s", split, got, tc.want)
				}
			}
		})
	}
}

// Rounds of another download with the same album tag in a folder of the same name are another
// album: the album of a folder is the one its own download made.
func TestOtherDownloadIsAnotherAlbum(t *testing.T) {
	ctx := context.Background()
	im, _, _ := setup(t)
	for i, artist := range []string{"A", "B"} {
		root := t.TempDir()
		p := filepath.Join(root, "Collection", "01.mp3")
		taggedMP3(t, p, map[string]string{"TIT2": "Song " + artist, "TPE1": artist, "TALB": "Collection", "TRCK": fmt.Sprint(i + 1)})
		b, _, _ := im.CreateBatchFiles(ctx, "download", "Box", root, []string{p}, false)
		runUntilDone(t, im, b)
	}
	if got := albumsNow(t, im); strings.Join(got, ";") != "Collection | A | 1;Collection | B | 1" {
		t.Fatalf("albums %v", got)
	}
}

// Importing again some of the songs of an album whose album artist was worked out finds their
// entries, whatever album artist those songs alone would give (review #81).
func TestSubsetImportedAgainStays(t *testing.T) {
	ctx := context.Background()
	im, _, _ := setup(t)
	src := t.TempDir()
	taggedMP3(t, filepath.Join(src, "Collection", "01.mp3"), map[string]string{"TIT2": "One", "TPE1": "A", "TALB": "Collection", "TRCK": "1"})
	taggedMP3(t, filepath.Join(src, "Collection", "02.mp3"), map[string]string{"TIT2": "Two", "TPE1": "B", "TALB": "Collection", "TRCK": "2"})
	b, _, _ := im.CreateBatch(ctx, "local", "", src, false)
	runUntilDone(t, im, b)
	again, _, _ := im.CreateBatchFiles(ctx, "local", "", src, []string{filepath.Join(src, "Collection", "01.mp3")}, false)
	if got := states(runUntilDone(t, im, again)); got["Collection/01.mp3"] != StateDuplicate {
		t.Fatalf("again: %v", got)
	}
	if got := albumsNow(t, im); len(got) != 1 || got[0] != "Collection | Various Artists | 2" {
		t.Fatalf("albums %v", got)
	}
}

// Merged by hand after a round, the album keeps the later rounds' songs of its folder (review #81).
func TestLaterRoundFollowsMerge(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	root := t.TempDir()
	one := filepath.Join(root, "Collection", "01.mp3")
	two := filepath.Join(root, "Collection", "02.mp3")
	taggedMP3(t, one, map[string]string{"TIT2": "One", "TPE1": "A", "TALB": "Collection", "TRCK": "1"})
	taggedMP3(t, two, map[string]string{"TIT2": "Two", "TPE1": "B", "TALB": "Collection", "TRCK": "2"})
	other := filepath.Join(t.TempDir(), "Box", "x.mp3")
	taggedMP3(t, other, map[string]string{"TIT2": "X", "TPE1": "Z", "TALB": "The Box", "TPE2": "Z", "TRCK": "9"})
	b, _, _ := im.CreateBatchFiles(ctx, "local", "", filepath.Dir(filepath.Dir(other)), []string{other}, false)
	runUntilDone(t, im, b)
	b, _, _ = im.CreateBatchFiles(ctx, "download", "Box", root, []string{one}, false)
	runUntilDone(t, im, b)
	albums, _ := lib.Albums(ctx, 10, 0, false)
	ids := map[string]int64{}
	for _, a := range albums {
		ids[a.Title] = a.ID
	}
	if _, err := lib.MergeAlbum(ctx, ids["Collection"], ids["The Box"]); err != nil {
		t.Fatal(err)
	}
	b, _, _ = im.CreateBatchFiles(ctx, "download", "Box", root, []string{two}, false)
	runUntilDone(t, im, b)
	if got := albumsNow(t, im); len(got) != 1 || got[0] != "The Box | Z | 3" {
		t.Fatalf("albums %v", got)
	}
}
