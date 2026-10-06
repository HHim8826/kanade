package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/HHim8826/kanade/server/internal/bangumi"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/thumbs"
)

// Works and Bangumi (review #94): searching Bangumi for a work to link an album to, linking and
// unlinking it, what songs are to works, and the works' pages. Only searching, linking and
// refreshing ask Bangumi; what the library shows comes from what it kept.

// workStale is how old a work's description gets before its page reads it again, in the background.
const workStale = 30 * 24 * time.Hour

// candidate is a Bangumi subject offered for an album.
type candidate struct {
	SourceID string  `json:"source_id"`
	Type     int     `json:"type"`
	Name     string  `json:"name"`
	NameCN   string  `json:"name_cn,omitempty"`
	Platform string  `json:"platform,omitempty"`
	Date     string  `json:"date,omitempty"`
	Summary  string  `json:"summary,omitempty"`
	Score    float64 `json:"score,omitempty"`
	Rank     int     `json:"rank,omitempty"`
	Votes    int     `json:"votes,omitempty"`
	URL      string  `json:"url"`
	Image    bool    `json:"image"`
	NSFW     bool    `json:"nsfw,omitempty"`
	WorkID   int64   `json:"work_id,omitempty"` // kept as this work already
	Linked   bool    `json:"linked"`            // the album asked about is linked to it
	Subject  bool    `json:"subject"`           // the album asked about is this subject
	ByID     bool    `json:"by_id,omitempty"`   // found by the number asked for, not by name
}

func workData(sub *bangumi.Subject) library.WorkData {
	d := library.WorkData{Source: library.SourceBangumi, SourceID: strconv.FormatInt(sub.ID, 10), Type: sub.Type, Name: sub.Name,
		NameCN: sub.NameCN, Platform: sub.Platform, Date: sub.Date, Summary: sub.Summary, Image: sub.Image()}
	if sub.Rating != nil && sub.Rating.Total > 0 {
		d.Score, d.Rank, d.Votes = sub.Rating.Score, sub.Rating.Rank, sub.Rating.Total
	}
	return d
}

func (s *Server) bangumiError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, bangumi.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, bangumi.ErrUnavailable):
		upstreamError(w, err, 30)
	case r.Context().Err() != nil:
	default:
		s.libError(w, r, err)
	}
}

// bangumiSearch finds subjects for ?q (words, a subject's link or its number) of ?types (numbers,
// comma-separated; all when none), from ?offset; ?album marks those it is linked to.
func (s *Server) bangumiSearch(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	q := strings.TrimSpace(qs.Get("q"))
	if q == "" || len([]rune(q)) > 200 {
		writeError(w, http.StatusBadRequest, errors.New("q is from 1 to 200 characters"))
		return
	}
	var types []int
	for _, p := range strings.Split(qs.Get("types"), ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || !(n == bangumi.Book || n == bangumi.Anime || n == bangumi.Music || n == bangumi.Game || n == bangumi.Real) {
			writeError(w, http.StatusBadRequest, errors.New("types are 1, 2, 3, 4 or 6"))
			return
		}
		types = append(types, n)
	}
	offset, _ := strconv.Atoi(qs.Get("offset"))
	album, _ := strconv.ParseInt(qs.Get("album"), 10, 64)
	ctx := r.Context()

	var found []bangumi.Subject
	byID := map[int64]bool{}
	total := 0
	id, isRef := bangumi.Ref(q)
	link := isRef && strings.Contains(q, "/")
	if isRef && offset == 0 { // a link names one subject; a number may name one, or be words ("86")
		sub, err := s.bgm.Subject(ctx, id)
		switch {
		case err == nil:
			found, byID[sub.ID] = append(found, *sub), true
			total = 1
		case errors.Is(err, bangumi.ErrNotFound) && !link:
		default:
			s.bangumiError(w, r, err)
			return
		}
	}
	if !link {
		page, err := s.bgm.Search(ctx, q, types, offset)
		if err != nil {
			s.bangumiError(w, r, err)
			return
		}
		total += page.Total
		for _, sub := range page.Subjects {
			if !byID[sub.ID] {
				found = append(found, sub)
			}
		}
	}
	linked := map[int64]bool{}
	var own *library.Work
	if album > 0 {
		var err error
		if own, err = s.lib.AlbumSubject(ctx, album); err != nil {
			s.internal(w, r, err)
			return
		}
		works, err := s.lib.AlbumWorks(ctx, album)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		for _, wk := range works {
			linked[wk.ID] = true
		}
	}
	out := []candidate{}
	for _, sub := range found {
		c := candidate{SourceID: strconv.FormatInt(sub.ID, 10), Type: sub.Type, Name: sub.Name, NameCN: sub.NameCN, Platform: sub.Platform,
			Date: sub.Date, Summary: sub.Summary, URL: s.bgm.URL(sub.ID), Image: sub.Image() != "", NSFW: sub.NSFW, ByID: byID[sub.ID]}
		if sub.Rating != nil && sub.Rating.Total > 0 {
			c.Score, c.Rank, c.Votes = sub.Rating.Score, sub.Rating.Rank, sub.Rating.Total
		}
		if wk, err := s.lib.WorkFor(ctx, library.SourceBangumi, c.SourceID); err != nil {
			s.internal(w, r, err)
			return
		} else if wk != nil {
			c.WorkID, c.Linked = wk.ID, linked[wk.ID]
		}
		c.Subject = own != nil && own.SourceID == c.SourceID
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "offset": offset, "page_size": bangumi.PageSize, "subjects": out})
}

// bangumiImage is a subject's picture, for a candidate not kept yet.
func (s *Server) bangumiImage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("bad subject"))
		return
	}
	u, err := s.bgm.ImageOf(r.Context(), id)
	if err != nil {
		s.bangumiError(w, r, err)
		return
	}
	s.pictureOf(w, r, u)
}

// workImage is a work's picture.
func (s *Server) workImage(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u, err := s.lib.WorkImage(r.Context(), id)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	s.pictureOf(w, r, u)
}

// pictureOf serves one of Bangumi's pictures at ?size, made once and kept with the covers.
func (s *Server) pictureOf(w http.ResponseWriter, r *http.Request, u string) {
	if u == "" {
		writeError(w, http.StatusNotFound, errors.New("no picture"))
		return
	}
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	if size = thumbs.Size(size); size == 0 {
		size = thumbs.Size(600)
	}
	sum := sha256.Sum256([]byte(u))
	data, err := s.thumbs.Get(r.Context(), "bgm-"+hex.EncodeToString(sum[:20]), size, func(ctx context.Context) (io.ReadCloser, error) {
		return s.bgm.OpenImage(ctx, u)
	})
	switch {
	case errors.Is(err, thumbs.ErrUndecodable), errors.Is(err, bangumi.ErrImage), errors.Is(err, bangumi.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
		return
	case err != nil:
		if r.Context().Err() == nil {
			upstreamError(w, err, 60)
		}
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(data)
}

// works lists the works the library is linked to, of ?type, by name or ?sort=date.
func (s *Server) works(w http.ResponseWriter, r *http.Request) {
	typ, _ := strconv.Atoi(r.URL.Query().Get("type"))
	list, err := s.lib.Works(r.Context(), typ, r.URL.Query().Get("sort") == "date")
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// work is a work's page: what it is, and the library's albums and songs of it. A description read
// long ago is read again in the background; the page does not wait for Bangumi.
func (s *Server) work(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	d, err := s.lib.Work(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if d == nil {
		writeError(w, http.StatusNotFound, errors.New("no such work"))
		return
	}
	if time.Since(time.UnixMilli(d.Work.FetchedAt)) > workStale {
		if _, busy := s.refreshing.LoadOrStore(id, true); !busy {
			go func() {
				defer s.refreshing.Delete(id)
				ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
				defer cancel()
				if err := s.refreshWork(ctx, id); err != nil {
					s.log.Info("work not refreshed", "work", id, "err", err)
				}
			}()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"work": d.Work, "albums": d.Albums, "songs": d.Songs,
		"url": s.workURL(d.Work), "stale": time.Since(time.UnixMilli(d.Work.FetchedAt)) > workStale})
}

func (s *Server) workURL(wk library.Work) string {
	id, _ := strconv.ParseInt(wk.SourceID, 10, 64)
	return s.bgm.URL(id)
}

// refreshWork reads a work's description again.
func (s *Server) refreshWork(ctx context.Context, id int64) error {
	source, sid, _, err := s.lib.WorkSource(ctx, id)
	if err != nil {
		return err
	}
	n, err := strconv.ParseInt(sid, 10, 64)
	if source != library.SourceBangumi || err != nil {
		return library.ErrNotFound
	}
	s.bgm.Forget(n)
	sub, err := s.bgm.Subject(ctx, n)
	if err != nil {
		return err
	}
	_, err = s.lib.PutWork(ctx, workData(sub))
	return err
}

// refreshWorkNow reads a work's description again, on request.
func (s *Server) refreshWorkNow(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.refreshWork(r.Context(), id); err != nil {
		s.bangumiError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// linkWork links an album to a Bangumi subject (source_id: its number or link), kept as a work;
// replace, a work the album is linked to, gives its place. A work already kept links while Bangumi
// cannot be reached.
func (s *Server) linkWork(w http.ResponseWriter, r *http.Request) {
	album, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		SourceID string `json:"source_id"`
		Replace  int64  `json:"replace"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	work, ok := s.keepWork(w, r, req.SourceID)
	if !ok {
		return
	}
	g, err := s.lib.LinkWork(r.Context(), album, work, req.Replace)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": g, "work_id": work})
}

// linkAlbums links several albums to one Bangumi subject at once: {albums, source_id}.
func (s *Server) linkAlbums(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Albums   []int64 `json:"albums"`
		SourceID string  `json:"source_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	work, ok := s.keepWork(w, r, req.SourceID)
	if !ok {
		return
	}
	g, err := s.lib.LinkAlbums(r.Context(), req.Albums, work)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": g, "work_id": work})
}

// keepWork keeps the Bangumi subject sourceID names (its number or link) as a work, read again
// from Bangumi: the work's ID. One kept already serves while Bangumi cannot be reached. Otherwise
// the answer is written, and ok is false.
func (s *Server) keepWork(w http.ResponseWriter, r *http.Request, sourceID string) (work int64, ok bool) {
	sid, ok := bangumi.Ref(sourceID)
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("source_id is a Bangumi subject's number or link"))
		return 0, false
	}
	sub, err := s.bgm.Subject(r.Context(), sid)
	switch {
	case err == nil:
		if work, err = s.lib.PutWork(r.Context(), workData(sub)); err != nil {
			s.libError(w, r, err)
			return 0, false
		}
		return work, true
	case errors.Is(err, bangumi.ErrUnavailable):
		wk, err2 := s.lib.WorkFor(r.Context(), library.SourceBangumi, strconv.FormatInt(sid, 10))
		if err2 != nil || wk == nil {
			s.bangumiError(w, r, err)
			return 0, false
		}
		return wk.ID, true
	default:
		s.bangumiError(w, r, err)
		return 0, false
	}
}

func (s *Server) unlinkWork(w http.ResponseWriter, r *http.Request) {
	album, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	work, err := strconv.ParseInt(r.PathValue("work"), 10, 64)
	if err != nil || work <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("bad work"))
		return
	}
	g, err := s.lib.UnlinkWork(r.Context(), album, work)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": g})
}

// setTrackWorks says what a song is to works: {works: [{work_id, use, note}]}.
func (s *Server) setTrackWorks(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Works []library.TrackWork `json:"works"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.lib.SetTrackWorks(r.Context(), id, req.Works)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": g})
}
