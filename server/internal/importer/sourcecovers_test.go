package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HHim8826/kanade/server/internal/storage"
	"github.com/HHim8826/kanade/server/internal/storage/storagetest"
)

// coverSource is a source the cover tests change as its owner would: a folder, or a bucket.
type coverSource struct {
	song   func(p, title, album, artist string)
	put    func(p string, data []byte)
	remove func(p string)
	rename func(from, to string) // a folder and what is below it
}

func pictureOf(fill byte) []byte {
	return append(append([]byte{}, jpeg[:11]...), bytes.Repeat([]byte{fill}, 64+int(fill))...)
}

type folderCover struct {
	id       int64
	ref, sha string
}

// A source's pictures taken as covers are followed by its scans (review #221): a folder renamed or
// an album moved elsewhere has its cover read from where its picture is now; other bytes in a
// picture's place are a cover of their own (another ID and checksum, so nothing made of the old
// one, its sizes or a browser's copy, passes for it) for its folder's album, while the album beside
// it with the old picture keeps that; a picture gone has its cover read from another copy, or its
// album has none. Covers taken before the scans followed them are found too.
func followCoverCases(t *testing.T, im *Importer, set *storage.Set, st *storage.Store, src coverSource) {
	ctx := context.Background()
	kind := st.Kind
	ref := func(key string) string { return storage.Ref(kind, st.ID, key) }
	x, y, z := pictureOf(1), pictureOf(2), pictureOf(3)
	src.song("Aimer/Walpurgis/01.mp3", "Walpurgis One", "Walpurgis", "Aimer")
	src.song("Aimer/Walpurgis/02.mp3", "Walpurgis Two", "Walpurgis", "Aimer")
	src.put("Aimer/Walpurgis/cover.jpg", x)
	src.song("Kalafina/Red Moon/01.mp3", "Red Moon One", "Red Moon", "Kalafina")
	src.put("Kalafina/Red Moon/cover.jpg", x) // the same picture
	src.song("Band/Single/01.mp3", "Single One", "Single", "Band")
	src.put("Band/Single/cover.jpg", z)
	scan := func(what string) ScanResult {
		t.Helper()
		res, err := im.ScanSource(ctx, set, st.ID, false)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if res.Batch != 0 {
			runUntilDone(t, im, res.Batch)
		}
		return res
	}
	covers := func() map[string]folderCover {
		t.Helper()
		rows, err := im.db.Query(`SELECT al.title, coalesce(c.id, 0), coalesce(c.file_ref, ''), coalesce(c.sha256, '') FROM albums al
			LEFT JOIN covers c ON c.id = al.cover_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]folderCover{}
		for rows.Next() {
			var title string
			var c folderCover
			rows.Scan(&title, &c.id, &c.ref, &c.sha)
			out[title] = c
		}
		return out
	}
	// readable checks every cover's picture is where it says, with its bytes.
	readable := func(what string) {
		t.Helper()
		rows, _ := im.db.Query(`SELECT file_ref, sha256 FROM covers`)
		defer rows.Close()
		for rows.Next() {
			var r, sha string
			rows.Scan(&r, &sha)
			rc, err := set.Open(ctx, r, 0, -1)
			if err != nil {
				t.Fatalf("%s: cover %s: %v", what, r, err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			if sha256Hex(b) != sha {
				t.Fatalf("%s: cover %s holds other bytes", what, r)
			}
		}
	}
	tracked := func() string {
		t.Helper()
		var keys string
		im.db.QueryRow(`SELECT coalesce(group_concat(key, ','), '') FROM (SELECT key FROM source_covers WHERE store_id = ? ORDER BY key)`, st.ID).Scan(&keys)
		return keys
	}

	scan("first")
	c := covers()
	if c["Walpurgis"].id == 0 || c["Walpurgis"].id != c["Red Moon"].id || c["Walpurgis"].sha != sha256Hex(x) || c["Single"].sha != sha256Hex(z) {
		t.Fatalf("covers %+v", c)
	}
	if want := "Aimer/Walpurgis/cover.jpg,Band/Single/cover.jpg,Kalafina/Red Moon/cover.jpg"; tracked() != want {
		t.Fatalf("followed %q", tracked())
	}
	readable("first")
	if res := scan("nothing new"); res.Covers != 0 {
		t.Fatalf("nothing new: %+v", res)
	}

	// The folder renamed: the songs and the picture followed.
	src.rename("Aimer/Walpurgis", "Aimer/Walpurgis (2024)")
	// The picture is new to the gallery's tracking (#134): only a JPEG's first bytes, it was no picture
	// a gallery takes, so it is read again where it is now.
	if res := scan("renamed"); res.Moved != 2 || res.Covers != 1 || res.New != 1 {
		t.Fatalf("renamed: %+v", res)
	}
	if now := covers(); now["Walpurgis"].id != c["Walpurgis"].id || now["Red Moon"].id != c["Red Moon"].id {
		t.Fatalf("after the rename %+v", now)
	}
	if want := "Aimer/Walpurgis (2024)/cover.jpg,Band/Single/cover.jpg,Kalafina/Red Moon/cover.jpg"; tracked() != want {
		t.Fatalf("followed %q", tracked())
	}
	readable("renamed")
	// The album moved into another folder.
	src.rename("Band/Single", "Collection/Band/Single")
	if res := scan("moved"); res.Moved != 1 || res.Covers != 1 {
		t.Fatalf("moved: %+v", res)
	}
	if now := covers(); now["Single"].id != c["Single"].id || now["Single"].ref != ref("Collection/Band/Single/cover.jpg") {
		t.Fatalf("after the move %+v", now)
	}
	readable("moved")

	// Other bytes: Walpurgis gets a cover of its own; Red Moon keeps the old picture, read from its copy.
	src.put("Aimer/Walpurgis (2024)/cover.jpg", y)
	if res := scan("other bytes"); res.Covers != 1 || res.Changed != 1 { // the picture, read again for the gallery (#134)
		t.Fatalf("other bytes: %+v", res)
	}
	now := covers()
	if w := now["Walpurgis"]; w.id == c["Walpurgis"].id || w.sha != sha256Hex(y) || w.ref != ref("Aimer/Walpurgis (2024)/cover.jpg") {
		t.Fatalf("Walpurgis after its picture changed: %+v (was %+v)", w, c["Walpurgis"])
	}
	if r := now["Red Moon"]; r.id != c["Red Moon"].id || r.sha != sha256Hex(x) || r.ref != ref("Kalafina/Red Moon/cover.jpg") {
		t.Fatalf("Red Moon after Walpurgis's picture changed: %+v", r)
	}
	readable("other bytes")

	// Gone: Single has none, and its cover goes; Red Moon's last copy gone too.
	src.remove("Collection/Band/Single/cover.jpg")
	src.remove("Kalafina/Red Moon/cover.jpg")
	if res := scan("gone"); res.Covers != 2 {
		t.Fatalf("gone: %+v", res)
	}
	now = covers()
	if now["Single"].id != 0 || now["Red Moon"].id != 0 || now["Walpurgis"].sha != sha256Hex(y) {
		t.Fatalf("after the pictures went: %+v", now)
	}
	var left int
	im.db.QueryRow(`SELECT count(*) FROM covers WHERE id IN (?, ?)`, c["Single"].id, c["Red Moon"].id).Scan(&left)
	if left != 0 || tracked() != "Aimer/Walpurgis (2024)/cover.jpg" {
		t.Fatalf("%d covers left of those gone, followed %q", left, tracked())
	}
	readable("gone")

	// Taken before the scans followed covers: found by its name where it went.
	im.db.Exec(`DELETE FROM source_covers`)
	src.rename("Aimer/Walpurgis (2024)", "Aimer/Walpurgis [FLAC]")
	if res := scan("followed from before"); res.Moved != 2 || res.Covers != 1 {
		t.Fatalf("a cover from before: %+v", res)
	}
	if w := covers()["Walpurgis"]; w.ref != ref("Aimer/Walpurgis [FLAC]/cover.jpg") || tracked() != "Aimer/Walpurgis [FLAC]/cover.jpg" {
		t.Fatalf("a cover from before: %+v, followed %q", w, tracked())
	}
	readable("from before")

	// A song imported with a picture the library has, read from a place that is not there: read
	// from here (review #221).
	w := covers()["Walpurgis"]
	im.db.Exec(`UPDATE covers SET file_ref = 'local:99:gone/cover.jpg' WHERE id = ?`, w.id)
	src.song("Aimer/Walpurgis Live/01.mp3", "Live One", "Walpurgis Live", "Aimer")
	src.put("Aimer/Walpurgis Live/cover.jpg", y)
	if res := scan("the same picture, new"); res.New != 2 { // the song and its picture
		t.Fatalf("a new album: %+v", res)
	}
	if now := covers(); now["Walpurgis Live"].id != w.id || now["Walpurgis"].ref != ref("Aimer/Walpurgis Live/cover.jpg") {
		t.Fatalf("the picture again, where the library's could not be read: %+v", now)
	}
	readable("again")
}

func TestSourceCoversFollowedInAFolder(t *testing.T) {
	im, _, _ := setup(t)
	set := withStores(t, im)
	root := t.TempDir()
	st := addFolder(t, set, root, storage.Source)
	followCoverCases(t, im, set, st, coverSource{
		song: func(p, title, album, artist string) {
			taggedMP3(t, filepath.Join(root, p), map[string]string{"TIT2": title, "TALB": album, "TPE1": artist, "TRCK": "1"})
		},
		put: func(p string, data []byte) {
			full := filepath.Join(root, p)
			os.MkdirAll(filepath.Dir(full), 0o755)
			if err := os.WriteFile(full, data, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		remove: func(p string) {
			if err := os.Remove(filepath.Join(root, p)); err != nil {
				t.Fatal(err)
			}
		},
		rename: func(from, to string) {
			os.MkdirAll(filepath.Dir(filepath.Join(root, to)), 0o755)
			if err := os.Rename(filepath.Join(root, from), filepath.Join(root, to)); err != nil {
				t.Fatal(err)
			}
		},
	})
}

func TestSourceCoversFollowedInABucket(t *testing.T) {
	ctx := context.Background()
	im, _, _ := setup(t)
	set := withStores(t, im)
	fake := storagetest.NewS3(t)
	cfg, _ := json.Marshal(storage.S3Config{Endpoint: fake.URL, Bucket: fake.Bucket, Prefix: "music"})
	sec, _ := json.Marshal(storage.S3Secret{AccessKey: fake.AccessKey, SecretKey: fake.SecretKey})
	st, err := set.Add(ctx, storage.NewStore{Kind: storage.KindS3, Name: "R2", Mode: storage.Source, Config: cfg, Secret: sec})
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	followCoverCases(t, im, set, st, coverSource{
		song: func(p, title, album, artist string) {
			f := filepath.Join(tmp, path.Base(p))
			taggedMP3(t, f, map[string]string{"TIT2": title, "TALB": album, "TPE1": artist, "TRCK": "1"})
			b, _ := os.ReadFile(f)
			fake.Put("music/"+p, b)
		},
		put:    func(p string, data []byte) { fake.Put("music/"+p, data) },
		remove: func(p string) { fake.Remove("music/" + p) },
		rename: func(from, to string) {
			for _, n := range fake.Names() {
				if rest, ok := strings.CutPrefix(n, "music/"+from+"/"); ok {
					fake.Put("music/"+to+"/"+rest, fake.Object(n).Data)
					fake.Remove(n)
				}
			}
		},
	})
}
