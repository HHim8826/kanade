package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

// A collection's sections are the folders below its top folder, in natural order, loose songs
// first; songs are numbered in path order within each (review #82).
func TestCollectionLayout(t *testing.T) {
	sections, slots := CollectionLayout([]string{
		"Coll/Episode 10/x.flac", "Coll/Episode 2/C/01.flac", "Coll/Episode 1/B/01.flac", "Coll/Episode 1/A/02.flac",
		"Coll/Episode 1/A/01.flac", "Coll/readme.txt", "Coll/loose.flac", "Coll/Episode 1/A/cover.jpg",
	})
	if !reflect.DeepEqual(sections, []string{"", "Episode 1", "Episode 2", "Episode 10"}) {
		t.Fatalf("sections %q", sections)
	}
	want := map[string][2]int{"Coll/loose.flac": {1, 1}, "Coll/Episode 1/A/01.flac": {2, 1}, "Coll/Episode 1/A/02.flac": {2, 2},
		"Coll/Episode 1/B/01.flac": {2, 3}, "Coll/Episode 2/C/01.flac": {3, 1}, "Coll/Episode 10/x.flac": {4, 1}}
	if !reflect.DeepEqual(slots, want) {
		t.Fatalf("slots %v", slots)
	}
}

// A download imported as a collection is one album whatever its tags say and whatever round
// brought which song: its folders are named sections, each song keeps its own artist (review #82).
func TestCollectionInRounds(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	root := t.TempDir()
	type song struct{ path, album, artist string }
	songs := []song{
		{"Coll/Episode 1/Red/01.mp3", "musicbox Red", "A"}, {"Coll/Episode 1/Red/02.mp3", "musicbox Red", "B"},
		{"Coll/Episode 1/Blue/01.mp3", "musicbox Blue", "C"}, {"Coll/Episode 2/Gold/01.mp3", "Golden", "D"},
	}
	var all []string
	for i, s := range songs {
		p := filepath.Join(root, filepath.FromSlash(s.path))
		taggedMP3(t, p, map[string]string{"TIT2": fmt.Sprintf("Song %d", i), "TPE1": s.artist, "TALB": s.album, "TRCK": "1"})
		all = append(all, s.path)
	}
	sections, slots := CollectionLayout(all)
	for _, round := range [][]int{{0, 3}, {1, 2}} {
		var paths []string
		g := Grouping{Mode: GroupCollection, Title: "The Collection", Sections: sections, Slots: map[string][2]int{}}
		for _, i := range round {
			paths = append(paths, filepath.Join(root, filepath.FromSlash(songs[i].path)))
			g.Slots[songs[i].path] = slots[songs[i].path]
		}
		b, _, err := im.CreateBatchFiles(ctx, "download", "Coll", root, paths, false)
		if err != nil {
			t.Fatal(err)
		}
		opts, _ := json.Marshal(map[string]any{"grouping": g})
		im.db.Exec(`UPDATE import_batches SET options = ? WHERE id = ?`, string(opts), b)
		runUntilDone(t, im, b)
	}
	if got := albumsNow(t, im); len(got) != 1 || got[0] != "The Collection | Various Artists | 4" {
		t.Fatalf("albums %v", got)
	}
	albums, _ := lib.Albums(ctx, 10, 0, false)
	d, _ := lib.Album(ctx, albums[0].ID)
	if d.Sections[1] != "Episode 1" || d.Sections[2] != "Episode 2" {
		t.Fatalf("sections %v", d.Sections)
	}
	got := map[string]string{}
	for _, e := range d.Entries {
		got[fmt.Sprintf("%d-%d", e.DiscNo, e.TrackNo)] = e.Title + "/" + e.Artist
	}
	// Episode 1 in path order: Blue/01, Red/01, Red/02.
	if got["1-1"] != "Song 2/C" || got["1-2"] != "Song 0/A" || got["1-3"] != "Song 1/B" || got["2-1"] != "Song 3/D" {
		t.Fatalf("entries %v", got)
	}
}

// A download grouped by folders makes one album of a folder whichever rounds bring its songs, the
// same album one import of the whole folder makes (review #86); an album renamed since keeps its name.
func TestFolderGroupingIndependentOfRounds(t *testing.T) {
	type song struct{ album, artist, albumArtist string }
	for _, tc := range []struct {
		name   string
		songs  []song
		rename string // the album's title is changed after the first round
		want   string
	}{
		{"different album tags", []song{{"Original Red", "A", "Ensemble"}, {"Original Blue", "B", "Ensemble"}}, "",
			"Episode 1 | Ensemble | 2"},
		{"one album tag", []song{{"Same", "A", "Ensemble"}, {"Same", "B", "Ensemble"}}, "", "Same | Ensemble | 2"},
		{"different artists", []song{{"Same", "A", ""}, {"Same", "B", ""}}, "", "Same | Various Artists | 2"},
		{"renamed between rounds", []song{{"Original Red", "A", "Ensemble"}, {"Original Blue", "B", "Ensemble"}}, "Mine",
			"Mine | Ensemble | 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			for _, split := range []bool{false, true} {
				if tc.rename != "" && !split {
					continue
				}
				im, lib, _ := setup(t)
				root := t.TempDir()
				var paths []string
				for i, s := range tc.songs {
					frames := map[string]string{"TIT2": fmt.Sprint("Song ", i), "TPE1": s.artist, "TALB": s.album, "TRCK": fmt.Sprint(i + 1)}
					if s.albumArtist != "" {
						frames["TPE2"] = s.albumArtist
					}
					p := filepath.Join(root, "Box", "Episode 1", fmt.Sprintf("%02d.mp3", i+1))
					taggedMP3(t, p, frames)
					paths = append(paths, p)
				}
				rounds := [][]string{paths}
				if split {
					rounds = [][]string{paths[:1], paths[1:]}
				}
				for n, these := range rounds {
					b, _, err := im.CreateBatchFiles(ctx, "download", "Box", root, these, false)
					if err != nil {
						t.Fatal(err)
					}
					opts, _ := json.Marshal(map[string]any{"grouping": Grouping{Mode: GroupFolders}})
					im.db.Exec(`UPDATE import_batches SET options = ? WHERE id = ?`, string(opts), b)
					runUntilDone(t, im, b)
					if n == 0 && tc.rename != "" {
						albums, _ := lib.Albums(ctx, 10, 0, false)
						if _, err := im.db.Exec(`UPDATE albums SET title = ? WHERE id = ?`, tc.rename, albums[0].ID); err != nil {
							t.Fatal(err)
						}
					}
				}
				if got := albumsNow(t, im); len(got) != 1 || got[0] != tc.want {
					t.Fatalf("in rounds %v: %v, want %s", split, got, tc.want)
				}
			}
		})
	}
}
