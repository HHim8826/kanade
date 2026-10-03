package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
)

// Batch changes of the library (review #83) and a download's songs as a collection (review #82).
// Every ID is checked before anything changes; a change that can be undone is one edit, answered
// with its group.

// mergeAlbums merges albums into one; with "preview", it only says what would happen.
func (s *Server) mergeAlbums(w http.ResponseWriter, r *http.Request) {
	var req struct {
		library.MergeRequest
		Preview bool `json:"preview"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Preview {
		p, err := s.lib.PlanMerge(r.Context(), req.MergeRequest)
		if err != nil {
			s.libError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
		return
	}
	album, g, err := s.lib.MergeAlbums(r.Context(), req.MergeRequest)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"group": g, "album_id": album})
}

func (s *Server) editAlbums(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Albums []int64 `json:"albums"`
		library.AlbumFields
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.lib.EditAlbums(r.Context(), req.Albums, req.AlbumFields)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeGroup(w, g)
}

// removeAlbums takes albums off the library as one edit, their songs staying; with delete_tracks,
// the songs only on those albums are then deleted for good (not undone), each reported.
func (s *Server) removeAlbums(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Albums       []int64 `json:"albums"`
		DeleteTracks bool    `json:"delete_tracks"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx := r.Context()
	var tracks []int64
	for _, id := range req.Albums {
		ids, err := s.lib.AlbumTrackIDs(ctx, id)
		if err != nil {
			s.internal(w, r, err)
			return
		}
		tracks = append(tracks, ids...)
	}
	g, err := s.lib.RemoveAlbums(ctx, req.Albums)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	out := map[string]any{"group": g}
	if req.DeleteTracks {
		res := s.deleteTracks(r, tracks, true)
		out["deleted"], out["failed"], out["trashed"], out["trash_failed"] = res.Deleted, res.Failed, res.Trashed, res.TrashFailed
	}
	writeJSON(w, http.StatusOK, out)
}

type deleted struct {
	Deleted     []int64           `json:"deleted"`
	Failed      map[string]string `json:"failed"` // song -> why
	Trashed     int               `json:"trashed"`
	TrashFailed int               `json:"trash_failed"`
}

// deleteTracks deletes songs for good, each on its own; onlyLoose leaves songs still on an album.
func (s *Server) deleteTracks(r *http.Request, ids []int64, onlyLoose bool) deleted {
	ctx := r.Context()
	out := deleted{Deleted: []int64{}, Failed: map[string]string{}}
	var files []string
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if onlyLoose {
			t, err := s.lib.Track(ctx, id)
			if err != nil || t == nil || len(t.Entries) > 0 {
				continue // gone, or still on another album
			}
		}
		f, err := s.lib.DeleteTrack(ctx, id)
		if err != nil {
			out.Failed[strconv.FormatInt(id, 10)] = err.Error()
			continue
		}
		files = append(files, f...)
		out.Deleted = append(out.Deleted, id)
	}
	out.Trashed, out.TrashFailed = s.trashFiles(r, files)
	return out
}

// removeTracks deletes songs for good (not undone); the answer lists those deleted and why others
// were not, so they can be tried again.
func (s *Server) removeTracks(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tracks []int64 `json:"tracks"`
	}
	if err := readJSON(r, &req); err != nil || len(req.Tracks) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("expected {tracks: [IDs]}"))
		return
	}
	writeJSON(w, http.StatusOK, s.deleteTracks(r, req.Tracks, false))
}

func (s *Server) editTracks(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tracks []int64 `json:"tracks"`
		library.TrackFields
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.lib.EditTracks(r.Context(), req.Tracks, req.TrackFields)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeGroup(w, g)
}

func (s *Server) placeSongs(w http.ResponseWriter, r *http.Request) {
	var req library.PlaceRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	album, g, err := s.lib.PlaceSongs(r.Context(), req)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"group": g, "album_id": album})
}

func (s *Server) setFavorites(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tracks []int64 `json:"tracks"`
		Albums []int64 `json:"albums"`
		On     bool    `json:"on"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.SetFavorites(r.Context(), req.Tracks, req.Albums, req.On); err != nil {
		s.libError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setSections names an album's discs ({"sections": {"1": "Episode 1"}}; "" takes a name away).
func (s *Server) setSections(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Sections map[string]string `json:"sections"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	names := map[int]string{}
	for k, v := range req.Sections {
		d, err := strconv.Atoi(k)
		if err != nil || d < 1 || d > 99 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("disc %q: 1 to 99", k))
			return
		}
		names[d] = v
	}
	g, err := s.lib.SetSections(r.Context(), id, names)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeGroup(w, g)
}

// ---- a download's songs as a collection (review #82) ----

// setGrouping chooses how a download's songs go into albums from its next round on.
func (s *Server) setGrouping(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var g importer.Grouping
	if err := readJSON(r, &g); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.downloads.SetGrouping(r.Context(), id, &g); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// collectionPlan says how a download's imported songs would become one collection.
func (s *Server) collectionPlan(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	q := r.URL.Query()
	p, err := s.downloads.CollectionPlan(r.Context(), id, q.Get("title"), q.Get("artist"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.lib.CheckPlan(r.Context(), p); err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// makeCollection arranges a download's imported songs into one collection, as one edit, and has
// its later rounds (and songs fetched again) join it.
func (s *Server) makeCollection(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Title  string `json:"title"`
		Artist string `json:"artist"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx := r.Context()
	p, err := s.downloads.CollectionPlan(ctx, id, req.Title, req.Artist)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	album, g, err := s.lib.Arrange(ctx, p, fmt.Sprintf("將下載整理成合集「%s」（%d 首）", p.Target.Title, len(p.Moves)+len(p.Adds)))
	if err != nil {
		s.libError(w, r, err)
		return
	}
	err = s.downloads.SetGrouping(ctx, id, &importer.Grouping{Mode: importer.GroupCollection, Title: p.Target.Title, Artist: p.Target.AlbumArtist})
	if err == nil {
		err = s.importer.SetScopeAlbum(ctx, importer.CollectionScope(s.downloads.Dir(ctx, id), p.Target.Title), album, p.Target.AlbumArtist)
	}
	if err != nil {
		s.log.Warn("remember the collection for later rounds", "download", id, "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]int64{"group": g, "album_id": album})
}
