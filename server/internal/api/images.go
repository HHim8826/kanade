package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"

	"github.com/HHim8826/kanade/server/internal/config"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/thumbs"
)

// ---- an album's gallery (#134) ----

// albumImages lists an album's gallery, in order.
func (s *Server) albumImages(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	list, err := s.lib.AlbumImages(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": list})
}

// arrangeImages sets an album's gallery: every picture in the order wanted, with what each is and
// its disc.
func (s *Server) arrangeImages(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Images []library.ImagePlace `json:"images"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.ArrangeImages(r.Context(), id, req.Images); err != nil {
		if errors.Is(err, library.ErrInvalid) {
			writeError(w, http.StatusConflict, errors.New("the gallery changed meanwhile: load it again"))
			return
		}
		s.libError(w, r, err)
		return
	}
	s.albumImages(w, r)
}

// addAlbumImage keeps a picture uploaded for an album's gallery: the request's body, named by name.
func (s *Server) addAlbumImage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	f, err := os.CreateTemp(s.cfg.Path(config.DirStaging), "image-*")
	if err != nil {
		s.internal(w, r, err)
		return
	}
	defer os.Remove(f.Name())
	n, err := io.Copy(f, io.LimitReader(r.Body, maxImageUpload+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if n > maxImageUpload {
		writeError(w, http.StatusRequestEntityTooLarge, importer.ErrNotImage)
		return
	}
	m, added, err := s.importer.AddAlbumImage(r.Context(), id, r.URL.Query().Get("name"), f.Name())
	switch {
	case errors.Is(err, importer.ErrNotImage):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, library.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case err != nil:
		upstreamError(w, err, 0)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"image": m, "added": added})
	}
}

const maxImageUpload = 128 << 20

// hideImage takes a picture out of its gallery.
func (s *Server) hideImage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	album, err := s.lib.HideImage(r.Context(), id)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"album_id": album})
}

// galleryImage is a picture in a gallery, as it is now: none for one taken out.
func (s *Server) galleryImage(w http.ResponseWriter, r *http.Request) *library.AlbumImage {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return nil
	}
	m, hidden, err := s.lib.Image(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return nil
	}
	if m == nil || hidden {
		writeError(w, http.StatusNotFound, errors.New("no such picture"))
		return nil
	}
	return m
}

// needsView says whether a picture is shown by its viewing copy (thumbs.ViewSize): one a browser
// cannot show (TIFF, BMP), or too large to show as it is.
func needsView(m *library.AlbumImage) bool {
	web := m.MIME == "image/jpeg" || m.MIME == "image/png" || m.MIME == "image/webp"
	return !web || max(m.Width, m.Height) > thumbs.ViewSize || m.Size > 16<<20
}

// image sends a picture of a gallery: at size (a thumbs.Size) or, for size=view, to look at: the
// picture itself, or its viewing copy.
func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	m := s.galleryImage(w, r)
	if m == nil {
		return
	}
	open := func(ctx context.Context) (io.ReadCloser, error) { return s.files.Open(ctx, m.Ref, 0, -1) }
	size := thumbs.ViewSize
	if q := r.URL.Query().Get("size"); q != "view" {
		n, _ := strconv.Atoi(q)
		if size = thumbs.Size(n); size == 0 {
			size = thumbs.ViewSize
		}
	}
	var data []byte
	var err error
	switch {
	case size == thumbs.ViewSize && !needsView(m): // as it is, read through: originals are not kept here
		body, err := open(r.Context())
		if err != nil {
			upstreamError(w, err, 0)
			return
		}
		defer body.Close()
		w.Header().Set("Content-Type", m.MIME)
		w.Header().Set("Content-Length", strconv.FormatInt(m.Size, 10))
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		io.Copy(w, io.LimitReader(body, m.Size))
		return
	case size != thumbs.ViewSize && needsView(m): // made from the viewing copy
		data, err = s.thumbs.Get(r.Context(), m.SHA256, size, func(ctx context.Context) (io.ReadCloser, error) {
			view, err := s.thumbs.Get(ctx, m.SHA256, thumbs.ViewSize, open)
			if err != nil {
				return nil, err
			}
			return io.NopCloser(bytes.NewReader(view)), nil
		})
	default:
		data, err = s.thumbs.Get(r.Context(), m.SHA256, size, open)
	}
	switch {
	case errors.Is(err, thumbs.ErrUndecodable):
		writeError(w, http.StatusNotFound, err)
		return
	case err != nil:
		if r.Context().Err() == nil {
			upstreamError(w, err, 0)
		}
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable") // a picture's content never changes
	w.Write(data)
}

// imageFile sends a picture's file as it was kept, to save.
func (s *Server) imageFile(w http.ResponseWriter, r *http.Request) {
	m := s.galleryImage(w, r)
	if m == nil {
		return
	}
	body, err := s.files.Open(r.Context(), m.Ref, 0, -1)
	if err != nil {
		upstreamError(w, err, 0)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", m.MIME)
	w.Header().Set("Content-Length", strconv.FormatInt(m.Size, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": m.Name}))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	io.Copy(w, io.LimitReader(body, m.Size))
}

// imageCover makes a picture its album's cover (as setting a cover does: in the edit log). A JPEG or
// PNG is the cover as it is; another, its viewing copy as a JPEG.
func (s *Server) imageCover(w http.ResponseWriter, r *http.Request) {
	m := s.galleryImage(w, r)
	if m == nil {
		return
	}
	album, _, err := s.lib.AlbumNow(r.Context(), m.AlbumID)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if album == 0 {
		writeError(w, http.StatusNotFound, errors.New("the picture's album is gone"))
		return
	}
	var cover int64
	if (m.MIME == "image/jpeg" || m.MIME == "image/png") && m.Size <= 16<<20 {
		cover, err = s.lib.ImageCover(r.Context(), m)
	} else {
		var view []byte
		view, err = s.thumbs.Get(r.Context(), m.SHA256, thumbs.ViewSize, func(ctx context.Context) (io.ReadCloser, error) {
			return s.files.Open(ctx, m.Ref, 0, -1)
		})
		if errors.Is(err, thumbs.ErrUndecodable) {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err == nil {
			cover, err = s.importer.StoreCover(r.Context(), view)
		}
	}
	if err != nil {
		upstreamError(w, err, 0)
		return
	}
	g, err := s.lib.SetAlbumCover(r.Context(), album, cover)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"group": g, "cover_id": cover, "album_id": album})
}
