package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/HHim8826/kanade/server/internal/library"
	"golang.org/x/image/tiff"
)

func pictureData(t *testing.T, format string, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		img.Set(x, x%h, color.RGBA{uint8(x), uint8(w), 160, 255})
	}
	var buf bytes.Buffer
	var err error
	if format == "tiff" {
		err = tiff.Encode(&buf, img, nil)
	} else {
		err = jpeg.Encode(&buf, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// An album's gallery (#134): pictures uploaded (one already there listed as it is, one that is no
// picture refused), arranged (an old list refused), shown as they are or by a viewing copy, small,
// downloaded, made the cover, and taken out (its file to the trash unless the cover uses it); one
// taken out and uploaded again is back.
func TestAlbumGallery(t *testing.T) {
	s, h, token := storageServer(t)
	ctx := context.Background()
	cfg, _ := json.Marshal(map[string]string{"root": t.TempDir()})
	if rec := do(t, h, "POST", "/api/v1/storage", token, map[string]any{"kind": "local", "name": "Library", "mode": "managed", "config": json.RawMessage(cfg)}); rec.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "PUT", "/api/v1/storage/default", token, map[string]any{"id": "1"}); rec.Code >= 300 {
		t.Fatalf("default: %d %s", rec.Code, rec.Body)
	}
	a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: "c1", Size: 1, Format: "flac", Codec: "flac"})
	s.lib.MarkVerified(ctx, a.ID, "d-c1")
	pub, _ := s.lib.Publish(ctx, a.ID, library.EntryInput{Title: "t", Artist: "x", Album: "A", AlbumArtist: "y"})
	var album int64
	s.db.QueryRow(`SELECT album_id FROM album_entries WHERE id = ?`, pub.EntryID).Scan(&album)

	upload := func(album int64, name string, data []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/albums/%d/images?name=%s", album, url.QueryEscape(name)), bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/octet-stream")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	type added struct {
		Image library.AlbumImage
		Added bool
	}
	scan, back := pictureData(t, "jpeg", 300, 200), pictureData(t, "tiff", 400, 100)
	front := decode[added](t, upload(album, `Scans\01 front.jpg`, scan).Body)
	if !front.Added || front.Image.Name != "01 front.jpg" || front.Image.Kind != "front" || front.Image.Width != 300 || front.Image.MIME != "image/jpeg" {
		t.Fatalf("uploaded %+v", front)
	}
	if again := decode[added](t, upload(album, "other.jpg", scan).Body); again.Added || again.Image.ID != front.Image.ID {
		t.Fatalf("the same again: %+v", again)
	}
	tif := decode[added](t, upload(album, "back.tif", back).Body)
	if !tif.Added || tif.Image.Kind != "back" || tif.Image.MIME != "image/tiff" || tif.Image.Height != 100 {
		t.Fatalf("tiff %+v", tif)
	}
	if rec := upload(album, "notes.jpg", []byte("not a picture")); rec.Code != http.StatusBadRequest {
		t.Fatalf("no picture: %d %s", rec.Code, rec.Body)
	}
	if rec := upload(album+100, "a.jpg", scan); rec.Code != http.StatusNotFound {
		t.Fatalf("no album: %d %s", rec.Code, rec.Body)
	}

	gallery := func() []library.AlbumImage {
		rec := do(t, h, "GET", fmt.Sprintf("/api/v1/albums/%d/images", album), token, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("gallery: %d %s", rec.Code, rec.Body)
		}
		return decode[struct{ Images []library.AlbumImage }](t, rec.Body).Images
	}
	names := func(list []library.AlbumImage) string {
		var out []string
		for _, m := range list {
			out = append(out, fmt.Sprintf("%s:%s:%d", m.Name, m.Kind, m.Disc))
		}
		return strings.Join(out, " ")
	}
	if got := names(gallery()); got != "01 front.jpg:front:0 back.tif:back:0" {
		t.Fatal(got)
	}
	if d := decode[library.AlbumDetail](t, do(t, h, "GET", fmt.Sprintf("/api/v1/albums/%d", album), token, nil).Body); d.Images != 2 {
		t.Fatalf("detail says %d", d.Images)
	}
	arrange := func(places ...library.ImagePlace) *httptest.ResponseRecorder {
		return do(t, h, "PUT", fmt.Sprintf("/api/v1/albums/%d/images", album), token, map[string]any{"images": places})
	}
	if rec := arrange(library.ImagePlace{ID: tif.Image.ID, Kind: "booklet", Disc: 1}); rec.Code != http.StatusConflict {
		t.Fatalf("an old list: %d %s", rec.Code, rec.Body)
	}
	if rec := arrange(library.ImagePlace{ID: tif.Image.ID, Kind: "cover"}, library.ImagePlace{ID: front.Image.ID}); rec.Code != http.StatusConflict {
		t.Fatalf("no such kind: %d %s", rec.Code, rec.Body)
	}
	rec := arrange(library.ImagePlace{ID: tif.Image.ID, Kind: "booklet"}, library.ImagePlace{ID: front.Image.ID, Kind: "disc", Disc: 1})
	if got := names(decode[struct{ Images []library.AlbumImage }](t, rec.Body).Images); rec.Code != 200 || got != "back.tif:booklet:0 01 front.jpg:disc:1" {
		t.Fatalf("arranged: %d %s", rec.Code, got)
	}

	get := func(path string) *httptest.ResponseRecorder { return do(t, h, "GET", path, token, nil) }
	if rec := get(fmt.Sprintf("/api/v1/images/%d?size=view", front.Image.ID)); rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), scan) || rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("a JPEG as it is: %d %s", rec.Code, rec.Header())
	}
	size := func(rec *httptest.ResponseRecorder) string {
		cfg, format, err := image.DecodeConfig(rec.Body)
		return fmt.Sprintf("%d %s %dx%d %v", rec.Code, format, cfg.Width, cfg.Height, err)
	}
	if got := size(get(fmt.Sprintf("/api/v1/images/%d?size=view", tif.Image.ID))); got != "200 jpeg 400x100 <nil>" {
		t.Fatalf("the TIFF to look at: %s", got)
	}
	if got := size(get(fmt.Sprintf("/api/v1/images/%d?size=96", tif.Image.ID))); got != "200 jpeg 96x24 <nil>" {
		t.Fatalf("the TIFF small: %s", got)
	}
	if got := size(get(fmt.Sprintf("/api/v1/images/%d?size=256", front.Image.ID))); got != "200 jpeg 256x170 <nil>" {
		t.Fatalf("the JPEG small: %s", got)
	}
	rec = get(fmt.Sprintf("/api/v1/images/%d/file", tif.Image.ID))
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), back) || rec.Header().Get("Content-Disposition") != `attachment; filename=back.tif` {
		t.Fatalf("download: %d %s", rec.Code, rec.Header())
	}

	// The JPEG as the cover is the file itself; the TIFF's, its viewing copy.
	cover := func(id int64) int64 {
		rec := do(t, h, "POST", fmt.Sprintf("/api/v1/images/%d/cover", id), token, nil)
		if rec.Code != 200 {
			t.Fatalf("cover: %d %s", rec.Code, rec.Body)
		}
		got := decode[struct {
			CoverID int64 `json:"cover_id"`
		}](t, rec.Body).CoverID
		if d := decode[library.AlbumDetail](t, get(fmt.Sprintf("/api/v1/albums/%d", album)).Body); d.CoverID != got {
			t.Fatalf("album's cover %d, set %d", d.CoverID, got)
		}
		return got
	}
	var ref, frontRef, tifRef string
	s.db.QueryRow(`SELECT file_ref FROM covers WHERE id = ?`, cover(front.Image.ID)).Scan(&ref)
	s.db.QueryRow(`SELECT file_ref FROM album_images WHERE id = ?`, front.Image.ID).Scan(&frontRef)
	s.db.QueryRow(`SELECT file_ref FROM album_images WHERE id = ?`, tif.Image.ID).Scan(&tifRef)
	if ref != frontRef {
		t.Fatalf("the JPEG's cover at %s, not its file %s", ref, frontRef)
	}
	if rec := do(t, h, "DELETE", fmt.Sprintf("/api/v1/images/%d", front.Image.ID), token, nil); rec.Code != 200 {
		t.Fatalf("take out: %d %s", rec.Code, rec.Body)
	}
	tifCover := cover(tif.Image.ID)
	var tifCoverMIME string
	s.db.QueryRow(`SELECT mime FROM covers WHERE id = ?`, tifCover).Scan(&tifCoverMIME)
	if tifCoverMIME != "image/jpeg" {
		t.Fatal("the TIFF's cover is", tifCoverMIME)
	}
	if rec := do(t, h, "DELETE", fmt.Sprintf("/api/v1/images/%d", tif.Image.ID), token, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), fmt.Sprint(album)) {
		t.Fatalf("take out: %d %s", rec.Code, rec.Body)
	}
	trashed := func(ref string) bool {
		var n int
		s.db.QueryRow(`SELECT count(*) FROM file_trash WHERE file_id = ?`, ref).Scan(&n)
		return n == 1
	}
	if trashed(frontRef) || !trashed(tifRef) {
		t.Fatalf("trash: the JPEG (a cover) %v, the TIFF %v", trashed(frontRef), trashed(tifRef))
	}
	if got := gallery(); len(got) != 0 {
		t.Fatal(names(got))
	}
	for _, p := range []string{"", "/file"} {
		if rec := get(fmt.Sprintf("/api/v1/images/%d%s", tif.Image.ID, p)); rec.Code != http.StatusNotFound {
			t.Fatalf("taken out%s: %d", p, rec.Code)
		}
	}
	if rec := do(t, h, "POST", fmt.Sprintf("/api/v1/images/%d/cover", tif.Image.ID), token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("cover of one taken out: %d", rec.Code)
	}
	// Uploaded again: back, kept where it was arranged.
	if back := decode[added](t, upload(album, "back again.tif", back).Body); !back.Added || back.Image.ID != tif.Image.ID {
		t.Fatalf("again: %+v", back)
	}
	if got := names(gallery()); got != "back.tif:booklet:0" {
		t.Fatal(got)
	}
}
