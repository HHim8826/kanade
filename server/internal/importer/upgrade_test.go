package importer

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/library"
)

// toVersion22 takes a database back to how migration 22 left it, the data as the importer of then
// wrote it: no album_scopes, no batch root, no derived marks in the plans.
func toVersion22(t *testing.T, d *sql.DB) {
	t.Helper()
	for _, q := range []string{
		`DROP TABLE album_scopes`,
		`ALTER TABLE import_batches DROP COLUMN root`,
		`ALTER TABLE downloads DROP COLUMN uploaded_before`,
		`DROP TABLE album_sections`,
		`ALTER TABLE downloads DROP COLUMN grouping`,
		`UPDATE import_items SET plan = json_remove(plan, '$.derived_artist', '$.tagged.Derived', '$.anchor.Derived') WHERE plan IS NOT NULL`,
		`DELETE FROM schema_migrations WHERE version > 22`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// A database from before album_scopes keeps telling an album artist worked out from the songs from
// one a tag named: the next round of the same download joins the album, as it does for data the
// current importer wrote (review #85).
func TestUpgradeKeepsDerivedAlbumArtist(t *testing.T) {
	type song struct{ artist, albumArtist string }
	for _, tc := range []struct {
		name         string
		first, later song
		want         []string
	}{
		{"worked out, a tag later", song{"A", ""}, song{"B", "Ensemble"}, []string{"Collection | Ensemble | 2"}},
		{"worked out, another artist later", song{"A", ""}, song{"B", ""}, []string{"Collection | Various Artists | 2"}},
		{"named by a tag", song{"A", "Ensemble"}, song{"B", ""}, []string{"Collection | Ensemble | 2"}},
		{"named, another named later", song{"A", "Ensemble"}, song{"B", "Other"},
			[]string{"Collection | Ensemble | 1", "Collection | Other | 1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "db.sqlite")
			d, err := db.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			fd := &fakeDrive{files: map[string][]byte{}}
			im := New(d, library.New(d), fd, t.TempDir(), log)
			root := t.TempDir()
			mp3 := func(n int, s song) string {
				frames := map[string]string{"TIT2": fmt.Sprint("Song ", n), "TPE1": s.artist, "TALB": "Collection", "TRCK": fmt.Sprint(n)}
				if s.albumArtist != "" {
					frames["TPE2"] = s.albumArtist
				}
				p := filepath.Join(root, "Collection", fmt.Sprintf("%02d.mp3", n))
				taggedMP3(t, p, frames)
				return p
			}
			b, _, err := im.CreateBatchFiles(ctx, "download", "Box", root, []string{mp3(1, tc.first)}, false)
			if err != nil {
				t.Fatal(err)
			}
			runUntilDone(t, im, b)
			if _, err := d.Exec(`INSERT INTO downloads (source, name, state, dir, files, import_batch_id, created_at, updated_at)
				VALUES ('magnet:', 'Box', 'downloading', ?, json_array(json_object('index', 1, 'path', 'Collection/01.mp3', 'batch', ?)), ?, 0, 0)`,
				root, b, b); err != nil {
				t.Fatal(err)
			}
			toVersion22(t, d)
			d.Close()

			if d, err = db.Open(ctx, path); err != nil { // migrations 23 on
				t.Fatal(err)
			}
			defer d.Close()
			var derived bool
			if err := d.QueryRow(`SELECT derived FROM album_scopes`).Scan(&derived); err != nil {
				t.Fatal(err)
			}
			if derived != (tc.first.albumArtist == "") {
				t.Fatalf("upgraded scope derived=%v", derived)
			}
			im = New(d, library.New(d), fd, t.TempDir(), log)
			b, _, err = im.CreateBatchFiles(ctx, "download", "Box", root, []string{mp3(2, tc.later)}, false)
			if err != nil {
				t.Fatal(err)
			}
			runUntilDone(t, im, b)
			if got := albumsNow(t, im); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("after the next round: %v, want %v", got, tc.want)
			}
		})
	}
}
