package importer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	stdjpeg "image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"

	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/storage"
)

// pictureIn is a w×h image of one colour in a format: jpeg, png, tiff or bmp.
func pictureIn(t *testing.T, format string, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		for y := range h {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	var err error
	switch format {
	case "jpeg":
		err = stdjpeg.Encode(&buf, img, nil)
	case "tiff":
		err = tiff.Encode(&buf, img, nil)
	case "bmp":
		err = bmp.Encode(&buf, img)
	default:
		return pngOf(t, c)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func put(t *testing.T, p string, data []byte) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func gallery(t *testing.T, lib *library.Store, album int64) []string {
	t.Helper()
	list, err := lib.AlbumImages(context.Background(), album)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range list {
		s := m.Path
		if m.Kind != "" {
			s += " " + m.Kind
		}
		if m.Disc > 0 {
			s += fmt.Sprintf(" disc%d", m.Disc)
		}
		out = append(out, s)
	}
	return out
}

func albumNamed(t *testing.T, lib *library.Store, title string) int64 {
	t.Helper()
	albums, _ := lib.Albums(context.Background(), 50, 0, false)
	for _, a := range albums {
		if a.Title == title {
			return a.ID
		}
	}
	t.Fatalf("no album %q", title)
	return 0
}

// An album's pictures (#134): those in its folder and the folders below it (a disc's too) are kept
// with it in natural order, what each is guessed from its name; another player's copies of the
// cover, the same bytes twice and pictures beside no album are not; TIFF and BMP scans are kept as
// they are.
func TestImportKeepsAnAlbumsPictures(t *testing.T) {
	ctx := context.Background()
	im, lib, fd := setup(t)
	src := t.TempDir()
	taggedMP3(t, filepath.Join(src, "Album/01.mp3"), map[string]string{"TIT2": "One", "TPE1": "A", "TALB": "Album", "TRCK": "1"})
	taggedMP3(t, filepath.Join(src, "Album/Disc 2/01.mp3"), map[string]string{"TIT2": "Two", "TPE1": "A", "TALB": "Album", "TRCK": "1", "TPOS": "2"})
	red := pictureIn(t, "jpeg", 40, 30, color.RGBA{200, 0, 0, 255})
	put(t, filepath.Join(src, "Album/cover.jpg"), red)
	put(t, filepath.Join(src, "Album/Scans/01.jpg"), red) // the same bytes
	put(t, filepath.Join(src, "Album/Scans/2.jpg"), pictureIn(t, "jpeg", 30, 40, color.RGBA{0, 200, 0, 255}))
	put(t, filepath.Join(src, "Album/Scans/10.jpg"), pictureIn(t, "jpeg", 30, 40, color.RGBA{0, 0, 200, 255}))
	put(t, filepath.Join(src, "Album/Scans/back.png"), pictureIn(t, "png", 4, 4, color.RGBA{9, 9, 9, 255}))
	put(t, filepath.Join(src, "Album/Scans/obi.tif"), pictureIn(t, "tiff", 12, 50, color.RGBA{1, 2, 3, 255}))
	put(t, filepath.Join(src, "Album/Disc 2/Scans/label.bmp"), pictureIn(t, "bmp", 20, 20, color.RGBA{4, 5, 6, 255}))
	put(t, filepath.Join(src, "Album/AlbumArtSmall.jpg"), pictureIn(t, "jpeg", 8, 8, color.RGBA{7, 7, 7, 255}))
	put(t, filepath.Join(src, "Album/Scans/broken.jpg"), []byte("not a picture"))
	put(t, filepath.Join(src, "Stray/art.jpg"), pictureIn(t, "jpeg", 8, 8, color.RGBA{8, 8, 8, 255}))

	batch, n, err := im.CreateBatch(ctx, "local", "", src, false)
	if err != nil || n != 2 {
		t.Fatalf("create: %d %v", n, err)
	}
	b := runUntilDone(t, im, batch)
	st := states(b)
	for rel, want := range map[string]string{"Album/cover.jpg": StateDuplicate, "Album/Scans/01.jpg": StatePublished,
		"Album/AlbumArtSmall.jpg": StateSkipped, "Album/Scans/broken.jpg": StateSkipped, "Stray/art.jpg": StateSkipped,
		"Album/Disc 2/Scans/label.bmp": StatePublished} {
		if st[rel] != want {
			t.Fatalf("%s: %s, want %s (%v)", rel, st[rel], want, st)
		}
	}
	album := albumNamed(t, lib, "Album")
	want := "Scans/01.jpg|Scans/2.jpg|Scans/10.jpg|Scans/back.png back|Scans/obi.tif obi|Disc 2/Scans/label.bmp disc disc2"
	if got := strings.Join(gallery(t, lib, album), "|"); got != want {
		t.Fatalf("gallery\n%s\nwant\n%s", got, want)
	}
	list, _ := lib.AlbumImages(ctx, album)
	obi := list[4]
	if obi.MIME != "image/tiff" || obi.Width != 12 || obi.Height != 50 || obi.Name != "obi.tif" {
		t.Fatalf("the TIFF: %+v", obi)
	}
	if data, ok := fd.files[obi.Ref]; !ok || !bytes.Equal(data, pictureIn(t, "tiff", 12, 50, color.RGBA{1, 2, 3, 255})) {
		t.Fatal("the TIFF is not kept as it was")
	}
	d, _ := lib.Album(ctx, album)
	if d.Images != 6 || d.CoverID == 0 {
		t.Fatalf("album %+v", d)
	}

	// Imported again: nothing twice.
	again, _, _ := im.CreateBatch(ctx, "local", "", src, false)
	runUntilDone(t, im, again)
	if got := len(gallery(t, lib, album)); got != 6 {
		t.Fatalf("after importing again: %d pictures", got)
	}
}

// The preview counts each album's pictures and can leave them out.
func TestPreviewLeavesPicturesOut(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	startWorker(t, im)
	src := t.TempDir()
	for _, a := range []string{"Kept", "Left"} {
		taggedMP3(t, filepath.Join(src, a, "01.mp3"), map[string]string{"TIT2": a, "TPE1": "A", "TALB": a, "TRCK": "1"})
		put(t, filepath.Join(src, a, "BK/01.jpg"), pictureIn(t, "jpeg", 8, 8, color.RGBA{byte(len(a)), 0, 0, 255}))
		put(t, filepath.Join(src, a, "BK/02.jpg"), pictureIn(t, "jpeg", 8, 8, color.RGBA{0, byte(len(a)), 0, 255}))
	}
	batch, _, err := im.CreateBatch(ctx, "local", "", src, true)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, im, batch, BatchReview)
	p, _ := im.Preview(ctx, batch)
	left := group(t, p, "Left")
	if left.Images != 2 || left.ImagesOff != 0 || group(t, p, "Kept").Images != 2 || len(p.Other) != 0 {
		t.Fatalf("preview %+v / %+v", left, p.Other)
	}
	if err := im.ApplyOp(ctx, batch, PlanOp{Op: "no_images", Group: left.Key}); err != nil {
		t.Fatal(err)
	}
	p, _ = im.Preview(ctx, batch)
	if g := group(t, p, "Left"); g.Images != 0 || g.ImagesOff != 2 || group(t, p, "Kept").Images != 2 {
		t.Fatalf("left out: %+v", g)
	}
	if err := im.ApplyOp(ctx, batch, PlanOp{Op: "no_images", Group: "g99"}); !errors.Is(err, ErrBadOp) {
		t.Fatalf("an album not there: %v", err)
	}
	im.Start(ctx, batch)
	waitState(t, im, batch, BatchDone)
	if n := len(gallery(t, lib, albumNamed(t, lib, "Left"))); n != 0 {
		t.Fatalf("left out, yet %d pictures", n)
	}
	if n := len(gallery(t, lib, albumNamed(t, lib, "Kept"))); n != 2 {
		t.Fatalf("kept %d pictures", n)
	}
}

// A gallery arranged, a picture taken out (its file to the trash, unless another album or a cover
// uses it), merged albums' galleries as one, and an upload: one taken out comes back.
func TestGalleryArrangedAndTakenOut(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	src := t.TempDir()
	taggedMP3(t, filepath.Join(src, "One/01.mp3"), map[string]string{"TIT2": "One", "TPE1": "A", "TALB": "One", "TRCK": "1"})
	taggedMP3(t, filepath.Join(src, "Two/01.mp3"), map[string]string{"TIT2": "Two", "TPE1": "A", "TALB": "Two", "TRCK": "1"})
	shared := pictureIn(t, "jpeg", 8, 8, color.RGBA{50, 50, 50, 255})
	put(t, filepath.Join(src, "One/a.jpg"), pictureIn(t, "jpeg", 8, 8, color.RGBA{1, 0, 0, 255}))
	put(t, filepath.Join(src, "One/b.jpg"), shared)
	put(t, filepath.Join(src, "Two/c.jpg"), pictureIn(t, "jpeg", 8, 8, color.RGBA{0, 1, 0, 255}))
	put(t, filepath.Join(src, "Two/folder.jpg"), shared) // the same picture in another album
	batch, _, _ := im.CreateBatch(ctx, "local", "", src, false)
	runUntilDone(t, im, batch)
	one, two := albumNamed(t, lib, "One"), albumNamed(t, lib, "Two")
	list, _ := lib.AlbumImages(ctx, one)
	if len(list) != 2 {
		t.Fatalf("one: %+v", list)
	}

	// Arranged: b first, said to be the back; a list out of date is refused.
	if err := lib.ArrangeImages(ctx, one, []library.ImagePlace{{ID: list[1].ID, Kind: "back"}, {ID: list[0].ID, Disc: 1}}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(gallery(t, lib, one), "|"); got != "b.jpg back|a.jpg disc1" {
		t.Fatalf("arranged %q", got)
	}
	for _, bad := range [][]library.ImagePlace{{{ID: list[0].ID}}, {{ID: list[0].ID}, {ID: list[0].ID}},
		{{ID: list[0].ID, Kind: "poster"}, {ID: list[1].ID}}} {
		if err := lib.ArrangeImages(ctx, one, bad); !errors.Is(err, library.ErrInvalid) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}

	// Taken out: b's file stays, Two uses the same one; a's goes to the trash.
	refs := map[string]string{}
	for _, m := range list {
		refs[m.Path] = m.Ref
	}
	trash := func() string {
		var ids string
		im.db.QueryRow(`SELECT coalesce(group_concat(file_id), '') FROM file_trash WHERE done_at = 0`).Scan(&ids)
		return ids
	}
	if album, err := lib.HideImage(ctx, list[1].ID); err != nil || album != one {
		t.Fatalf("hide: %d %v", album, err)
	}
	if album, err := lib.HideImage(ctx, list[0].ID); err != nil || album != one {
		t.Fatalf("hide: %d %v", album, err)
	}
	if got := gallery(t, lib, one); len(got) != 0 {
		t.Fatalf("taken out, yet %v", got)
	}
	if got := trash(); !strings.Contains(got, refs["a.jpg"]) || strings.Contains(got, refs["b.jpg"]) {
		t.Fatalf("owed to the trash: %q (a %s, b %s: Two's picture and cover)", got, refs["a.jpg"], refs["b.jpg"])
	}
	// Imported again: it stays out.
	again, _, _ := im.CreateBatch(ctx, "local", "", src, false)
	runUntilDone(t, im, again)
	if got := gallery(t, lib, one); len(got) != 0 {
		t.Fatalf("imported again, yet %v", got)
	}
	// Uploaded again: back.
	p := filepath.Join(t.TempDir(), "upload")
	put(t, p, pictureIn(t, "jpeg", 8, 8, color.RGBA{1, 0, 0, 255}))
	m, added, err := im.AddAlbumImage(ctx, one, "a.jpg", p)
	if err != nil || !added || m.ID != list[0].ID {
		t.Fatalf("uploaded again: %+v %v %v", m, added, err)
	}
	if got := strings.Join(gallery(t, lib, one), "|"); got != "a.jpg disc1" {
		t.Fatalf("after the upload %q", got)
	}
	// A new one, named by the upload; the same again is listed once.
	put(t, p, pictureIn(t, "png", 6, 9, color.RGBA{3, 3, 3, 255}))
	if m, added, err := im.AddAlbumImage(ctx, one, `C:\scans\Back Cover.png`, p); err != nil || !added || m.Kind != "back" || m.Path != "Back Cover.png" {
		t.Fatalf("a new one: %+v %v %v", m, added, err)
	}
	if _, added, err := im.AddAlbumImage(ctx, one, "again.png", p); err != nil || added {
		t.Fatalf("the same again: %v %v", added, err)
	}
	put(t, p, []byte("text"))
	if _, _, err := im.AddAlbumImage(ctx, one, "x.jpg", p); !errors.Is(err, ErrNotImage) {
		t.Fatalf("not a picture: %v", err)
	}

	// Merged: Two's gallery is One's too, the shared picture once.
	if _, err := lib.MergeAlbum(ctx, two, one); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(gallery(t, lib, one), "|"); got != "Back Cover.png back|c.jpg|folder.jpg front|a.jpg disc1" {
		t.Fatalf("merged %q", got)
	}
}

// A source's pictures are kept where they are, and an album imported before gets those that came
// since by the next scan.
func TestSourcePicturesInPlace(t *testing.T) {
	ctx := context.Background()
	im, lib, fd := setup(t)
	set := withStores(t, im)
	src := t.TempDir()
	taggedMP3(t, filepath.Join(src, "Band/Album/01.mp3"), map[string]string{"TIT2": "One", "TPE1": "Band", "TALB": "Album", "TRCK": "1"})
	st := addFolder(t, set, src, storage.Source)
	res, err := im.ScanSource(ctx, set, st.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	runUntilDone(t, im, res.Batch)
	album := albumNamed(t, lib, "Album")
	put(t, filepath.Join(src, "Band/Album/Scans/01.jpg"), pictureIn(t, "jpeg", 8, 8, color.RGBA{1, 1, 1, 255}))
	if res, err = im.ScanSource(ctx, set, st.ID, false); err != nil || res.New != 1 {
		t.Fatalf("second scan %+v %v", res, err)
	}
	runUntilDone(t, im, res.Batch)
	list, _ := lib.AlbumImages(ctx, album)
	if len(list) != 1 || list[0].Ref != storage.Ref(storage.KindLocal, st.ID, "Band/Album/Scans/01.jpg") || list[0].Path != "Scans/01.jpg" {
		t.Fatalf("gallery %+v", list)
	}
	if fd.uploads != 0 {
		t.Fatalf("%d uploads", fd.uploads)
	}
	// Taken out: the source's file stays; the next scans leave it out.
	lib.HideImage(ctx, list[0].ID)
	var owed int
	im.db.QueryRow(`SELECT count(*) FROM file_trash`).Scan(&owed)
	if _, err := os.Stat(filepath.Join(src, "Band/Album/Scans/01.jpg")); err != nil || owed != 0 {
		t.Fatalf("a source's picture: %v, %d owed to the trash", err, owed)
	}
	if res, _ := im.ScanSource(ctx, set, st.ID, false); res.New != 0 || len(gallery(t, lib, album)) != 0 {
		t.Fatalf("scanned again %+v", res)
	}
}

// A TIFF or BMP is measured by its header: one Go cannot decode (a YCbCr or CMYK TIFF, its
// directory after the picture, big-endian; an RLE BMP, top down) is kept all the same; one with no
// size, or cut short, is no picture.
func TestPictureSizes(t *testing.T) {
	tiffOf := func(bo binary.ByteOrder, w, h uint32, photometric uint16) []byte {
		var b bytes.Buffer
		if bo == binary.BigEndian {
			b.WriteString("MM\x00*")
		} else {
			b.WriteString("II*\x00")
		}
		pixels := bytes.Repeat([]byte{7}, 300)
		binary.Write(&b, bo, uint32(8+len(pixels))) // the directory after the picture
		b.Write(pixels)
		entry := func(tag, typ uint16, v uint32) {
			binary.Write(&b, bo, tag)
			binary.Write(&b, bo, typ)
			binary.Write(&b, bo, uint32(1))
			if typ == 3 {
				binary.Write(&b, bo, uint16(v))
				binary.Write(&b, bo, uint16(0))
			} else {
				binary.Write(&b, bo, v)
			}
		}
		binary.Write(&b, bo, uint16(4))
		entry(256, 4, w)
		entry(257, 3, h)
		entry(259, 3, 7) // JPEG inside
		entry(262, 3, uint32(photometric))
		binary.Write(&b, bo, uint32(0))
		return b.Bytes()
	}
	bmpOf := func(w, h int32, dib uint32) []byte {
		var b bytes.Buffer
		b.WriteString("BM")
		binary.Write(&b, binary.LittleEndian, []uint32{uint32(54 + 64), 0, 54, dib})
		binary.Write(&b, binary.LittleEndian, []int32{w, h})
		binary.Write(&b, binary.LittleEndian, []uint16{1, 8})
		binary.Write(&b, binary.LittleEndian, []uint32{1, 64, 0, 0, 0, 0}) // RLE8
		b.Write(bytes.Repeat([]byte{1}, 64))
		return b.Bytes()
	}
	for name, c := range map[string]struct {
		data       []byte
		mime, size string
	}{
		"ycbcr tiff":      {tiffOf(binary.LittleEndian, 3000, 1500, 6), "image/tiff", "3000x1500"},
		"cmyk tiff, MM":   {tiffOf(binary.BigEndian, 2480, 3508, 5), "image/tiff", "2480x3508"},
		"go's tiff":       {pictureIn(t, "tiff", 30, 20, color.White), "image/tiff", "30x20"},
		"rle bmp":         {bmpOf(640, -480, 40), "image/bmp", "640x480"},
		"go's bmp":        {pictureIn(t, "bmp", 31, 9, color.White), "image/bmp", "31x9"},
		"tiff, no size":   {tiffOf(binary.LittleEndian, 0, 10, 2), "", ""},
		"tiff, cut short": {tiffOf(binary.LittleEndian, 10, 10, 2)[:200], "", ""},
		"bmp, odd header": {bmpOf(10, 10, 20), "", ""},
	} {
		m, err := readImage(bytes.NewReader(c.data))
		if c.mime == "" {
			if !errors.Is(err, ErrNotImage) {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if got := fmt.Sprintf("%dx%d", m.Width, m.Height); err != nil || m.MIME != c.mime || got != c.size || m.Size != int64(len(c.data)) {
			t.Errorf("%s: %s %s %d %v", name, m.MIME, got, m.Size, err)
		}
	}
}

// A source's picture follows its file (review #238): other bytes there are another picture (a new
// ID), in its place in the gallery, arranged as it was, the old one gone; a picture whose file is
// gone is not listed nor read until it is back as it was; one taken out stays out.
func TestSourcePicturesFollowScans(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	set := withStores(t, im)
	src := t.TempDir()
	taggedMP3(t, filepath.Join(src, "Band/Album/01.mp3"), map[string]string{"TIT2": "One", "TPE1": "Band", "TALB": "Album", "TRCK": "1"})
	scan := filepath.Join(src, "Band/Album/Scans/01.jpg")
	put(t, scan, pictureIn(t, "jpeg", 64, 64, color.RGBA{200, 0, 0, 255}))
	st := addFolder(t, set, src, storage.Source)
	ref := storage.Ref(storage.KindLocal, st.ID, "Band/Album/Scans/01.jpg")
	at := time.Now().Add(-time.Hour)
	rescan := func(data []byte) {
		t.Helper()
		if data != nil {
			put(t, scan, data)
			at = at.Add(time.Minute) // another version
			os.Chtimes(scan, at, at)
		}
		res, err := im.ScanSource(ctx, set, st.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if res.Batch != 0 {
			runUntilDone(t, im, res.Batch)
		}
	}
	os.Chtimes(scan, at, at)
	rescan(nil)
	album := albumNamed(t, lib, "Album")
	only := func(what string) library.AlbumImage {
		t.Helper()
		list, err := lib.AlbumImages(ctx, album)
		if err != nil || len(list) != 1 {
			t.Fatalf("%s: %+v %v", what, list, err)
		}
		var n int
		im.db.QueryRow(`SELECT count(*) FROM album_images WHERE file_ref = ?`, ref).Scan(&n)
		if n != 1 {
			t.Fatalf("%s: %d pictures at the file", what, n)
		}
		return list[0]
	}
	first := only("imported")
	if err := lib.ArrangeImages(ctx, album, []library.ImagePlace{{ID: first.ID, Kind: "back"}}); err != nil {
		t.Fatal(err)
	}

	// Other bytes, another size: another picture in its place.
	rescan(pictureIn(t, "jpeg", 128, 64, color.RGBA{0, 200, 0, 255}))
	second := only("changed")
	if second.ID == first.ID || second.Width != 128 || second.SHA256 == first.SHA256 || second.Kind != "back" {
		t.Fatalf("changed: %+v, was %+v", second, first)
	}
	if m, _, _ := lib.Image(ctx, first.ID); m != nil {
		t.Fatalf("the old picture is still there: %+v", m)
	}
	// The same size, other bytes.
	rescan(pictureIn(t, "jpeg", 128, 64, color.RGBA{0, 0, 200, 255}))
	third := only("changed again")
	if third.ID == second.ID || third.SHA256 == second.SHA256 || third.Width != 128 {
		t.Fatalf("changed again: %+v", third)
	}
	data, _ := os.ReadFile(scan)

	// Gone: not listed, not read; back as it was: there again.
	os.Remove(scan)
	rescan(nil)
	if list, _ := lib.AlbumImages(ctx, album); len(list) != 0 {
		t.Fatalf("gone, still listed: %+v", list)
	}
	if _, out, _ := lib.Image(ctx, third.ID); !out {
		t.Fatal("gone, still read")
	}
	put(t, scan, data)
	os.Chtimes(scan, at, at)
	rescan(nil)
	if back := only("back"); back.ID != third.ID {
		t.Fatalf("back as %+v", back)
	}

	// Taken out, then other bytes there: still out.
	if _, err := lib.HideImage(ctx, third.ID); err != nil {
		t.Fatal(err)
	}
	rescan(pictureIn(t, "jpeg", 32, 32, color.RGBA{9, 9, 9, 255}))
	if list, _ := lib.AlbumImages(ctx, album); len(list) != 0 {
		t.Fatalf("taken out, then changed: listed %+v", list)
	}
	var hidden, n int
	im.db.QueryRow(`SELECT count(*), coalesce(sum(hidden), 0) FROM album_images WHERE file_ref = ?`, ref).Scan(&n, &hidden)
	if n != 1 || hidden != 1 {
		t.Fatalf("taken out, then changed: %d pictures at the file, %d out", n, hidden)
	}
}
