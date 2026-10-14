package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/config"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/staging"
	"github.com/HHim8826/kanade/server/internal/storage/storagetest"
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

// A picture taken out and uploaded again while its file is going to the trash is back with a file:
// the copy the upload found there, gone meanwhile, is kept again (review #241).
func TestGalleryUploadDuringTrash(t *testing.T) {
	for _, kind := range []string{"local", "s3"} {
		t.Run(kind, func(t *testing.T) {
			s, h, token := storageServer(t)
			ctx := context.Background()
			var watch atomic.Bool
			found := make(chan struct{})
			var once sync.Once
			body := map[string]any{"kind": "local", "name": "Library", "mode": "managed", "config": map[string]string{"root": t.TempDir()}}
			if kind == "s3" {
				fake := storagetest.NewS3(t)
				fake.Fail = func(r *http.Request) int {
					if watch.Load() && r.Method == "HEAD" && strings.Contains(r.URL.Path, "booklet") {
						once.Do(func() { close(found) })
					}
					return 0
				}
				body = map[string]any{"kind": "s3", "name": "R2", "mode": "managed", "config": map[string]string{"endpoint": fake.URL, "bucket": fake.Bucket},
					"secret": map[string]string{"access_key_id": fake.AccessKey, "secret_access_key": fake.SecretKey}}
			}
			if rec := do(t, h, "POST", "/api/v1/storage", token, body); rec.Code != http.StatusCreated {
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
			upload := func(data []byte) *httptest.ResponseRecorder {
				req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/albums/%d/images?name=booklet.jpg", album), bytes.NewReader(data))
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
			pic := pictureData(t, "jpeg", 64, 64)
			first := decode[added](t, upload(pic).Body)
			var ref string
			s.db.QueryRow(`SELECT file_ref FROM album_images WHERE id = ?`, first.Image.ID).Scan(&ref)
			if rec := do(t, h, "DELETE", fmt.Sprintf("/api/v1/images/%d", first.Image.ID), token, nil); rec.Code != 200 {
				t.Fatalf("take out: %d %s", rec.Code, rec.Body)
			}
			// Its file goes to the trash slowly; the upload finds it there meanwhile.
			trashing, release, trashed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				defer close(trashed)
				if n, err := s.lib.TrashFile(ctx, ref, func(ctx context.Context, ref string) error {
					close(trashing)
					<-release
					return s.files.Trash(ctx, ref)
				}); n != library.Trashed {
					t.Errorf("trash: %d %v", n, err)
				}
			}()
			<-trashing
			watch.Store(true)
			uploaded := make(chan *httptest.ResponseRecorder, 1)
			go func() { uploaded <- upload(pic) }()
			if kind == "s3" {
				<-found
			} else {
				time.Sleep(100 * time.Millisecond)
			}
			close(release)
			<-trashed
			rec := <-uploaded
			if again := decode[added](t, rec.Body); rec.Code != 200 || !again.Added || again.Image.ID != first.Image.ID {
				t.Fatalf("uploaded again: %d %+v", rec.Code, again)
			}
			if rec := do(t, h, "GET", fmt.Sprintf("/api/v1/images/%d/file", first.Image.ID), token, nil); rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), pic) {
				t.Fatalf("its file: %d %s", rec.Code, rec.Body)
			}
		})
	}
}

// probeReader calls f before each read.
type probeReader struct {
	r io.Reader
	f func()
}

func (p *probeReader) Read(b []byte) (int, error) {
	p.f()
	return p.r.Read(b)
}

// A picture uploaded is received under the staging budget: refused without room (its length told
// or not), nothing written; held while it is received, and let go after (review #239).
func TestGalleryUploadHoldsStaging(t *testing.T) {
	s, h, token := storageServer(t)
	ctx := context.Background()
	cfg, _ := json.Marshal(map[string]string{"root": t.TempDir()})
	if rec := do(t, h, "POST", "/api/v1/storage", token, map[string]any{"kind": "local", "name": "Library", "mode": "managed", "config": json.RawMessage(cfg)}); rec.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	do(t, h, "PUT", "/api/v1/storage/default", token, map[string]any{"id": "1"})
	a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: "c1", Size: 1, Format: "flac", Codec: "flac"})
	s.lib.MarkVerified(ctx, a.ID, "d-c1")
	pub, _ := s.lib.Publish(ctx, a.ID, library.EntryInput{Title: "t", Artist: "x", Album: "A", AlbumArtist: "y"})
	var album int64
	s.db.QueryRow(`SELECT album_id FROM album_entries WHERE id = ?`, pub.EntryID).Scan(&album)
	budget := &staging.Budget{Limit: 1, Reserve: 1, Free: func(string) int64 { return 0 }}
	s.staging = budget
	upload := func(body io.Reader) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/albums/%d/images?name=booklet.jpg", album), body)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/octet-stream")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	pic := pictureData(t, "jpeg", 64, 64)
	var written int64
	look := func() {
		files, _ := filepath.Glob(filepath.Join(s.cfg.Path(config.DirStaging), "image-*"))
		for _, f := range files {
			if st, err := os.Stat(f); err == nil {
				written = max(written, st.Size())
			}
		}
	}
	for _, body := range []io.Reader{bytes.NewReader(pic), &probeReader{bytes.NewReader(pic), look}} {
		if rec := upload(body); rec.Code != http.StatusInsufficientStorage {
			t.Fatalf("without room: %d %s", rec.Code, rec.Body)
		}
	}
	if written > 0 || budget.Used(ctx) != 0 {
		t.Fatalf("without room: %d bytes written, %d held", written, budget.Used(ctx))
	}
	budget.SetLimits(64<<20, 0)
	budget.Free = nil // room on the disk now
	var peak int64
	if rec := upload(&probeReader{bytes.NewReader(pic), func() { peak = max(peak, budget.Used(ctx)) }}); rec.Code != http.StatusOK {
		t.Fatalf("with room: %d %s", rec.Code, rec.Body)
	}
	if peak < int64(len(pic)) || budget.Used(ctx) != 0 {
		t.Fatalf("held %d while received (%d bytes), %d after", peak, len(pic), budget.Used(ctx))
	}
}
