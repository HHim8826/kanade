package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"

	"github.com/HHim8826/kanade/server/internal/gdrive"
	"github.com/HHim8826/kanade/server/internal/identify"
	"github.com/HHim8826/kanade/server/internal/importer"
	"github.com/HHim8826/kanade/server/internal/library"
)

// Organizing the library: edits, merge and split, restore, aliases, the edit log and undo (P2-2).

func writeGroup(w http.ResponseWriter, group int64) {
	writeJSON(w, http.StatusOK, map[string]int64{"group": group}) // 0: nothing changed
}

func (s *Server) track(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t, err := s.lib.Track(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if t == nil {
		writeError(w, http.StatusNotFound, library.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) editTrack(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req library.TrackEdit
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.lib.EditTrack(r.Context(), id, req)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeGroup(w, g)
}

// trashFiles moves files that left the library to the Drive trash. Each is owed to the trash in the
// database until this works; a failure is retried in the background (review #26).
// It goes through the library's trash lock, which also checks that no re-import took the file up
// again meanwhile (review #44).
func (s *Server) trashFiles(r *http.Request, ids []string) (trashed, failed int) {
	trash := func(ctx context.Context, id string) error {
		if err := s.drive.Trash(ctx, id); err != nil && !gdrive.IsNotFound(err) { // not found: already deleted in Drive
			return err
		}
		return nil
	}
	for _, id := range ids {
		switch n, err := s.lib.TrashFile(r.Context(), id, trash); n {
		case library.Trashed:
			trashed++
		case library.TrashFailedTry:
			s.log.Warn("trash drive file; will retry", "file", id, "err", err)
			failed++
		}
	}
	return trashed, failed
}

func (s *Server) deleteTrack(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	files, err := s.lib.DeleteTrack(r.Context(), id)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	trashed, failed := s.trashFiles(r, files)
	writeJSON(w, http.StatusOK, map[string]int{"trashed": trashed, "trash_failed": failed})
}

func (s *Server) restoreTrack(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.lib.RestoreTrack(r.Context(), id, s.importer.Original)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeGroup(w, g)
}

func (s *Server) editAlbum(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req library.AlbumEdit
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.lib.EditAlbum(r.Context(), id, req)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeGroup(w, g)
}

// setAlbumCover takes a JPEG or PNG as the request body.
func (s *Server) setAlbumCover(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 16<<20+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	cover, err := s.importer.StoreCover(r.Context(), data)
	if errors.Is(err, importer.ErrNotCover) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err != nil {
		upstreamError(w, err, 0)
		return
	}
	g, err := s.lib.SetAlbumCover(r.Context(), id, cover)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"group": g, "cover_id": cover})
}

func (s *Server) mergeAlbum(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Into int64 `json:"into"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.lib.MergeAlbum(r.Context(), id, req.Into)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeGroup(w, g)
}

func (s *Server) splitAlbum(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Title   string  `json:"title"`
		Entries []int64 `json:"entries"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	album, g, err := s.lib.SplitAlbum(r.Context(), id, req.Entries, req.Title)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"group": g, "album_id": album})
}

// removeEntries takes entries off an album; tracks stay as standalone songs.
func (s *Server) removeEntries(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Entries []int64 `json:"entries"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Entries) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("choose the entries to remove"))
		return
	}
	g, err := s.lib.RemoveEntries(r.Context(), id, req.Entries)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeGroup(w, g)
}

// deleteAlbum removes the album (undoable; its songs stay as standalone songs). With ?tracks=1 the
// songs that are on no other album are then deleted for good and their files go to the Drive trash;
// files other albums share are never touched (plan §8).
func (s *Server) deleteAlbum(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx := r.Context()
	d, err := s.lib.Album(ctx, id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if d == nil {
		writeError(w, http.StatusNotFound, library.ErrNotFound)
		return
	}
	tracks, err := s.lib.AlbumTrackIDs(ctx, id) // with the ones missing from Drive
	if err != nil {
		s.internal(w, r, err)
		return
	}
	g, err := s.lib.RemoveEntries(ctx, id, nil)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	out := map[string]int64{"group": g}
	if r.URL.Query().Get("tracks") == "1" {
		var files []string
		deleted := 0
		for _, tid := range tracks {
			t, err := s.lib.Track(ctx, tid)
			if err != nil {
				s.internal(w, r, err)
				return
			}
			if t == nil || len(t.Entries) > 0 {
				continue // already gone, or still on another album
			}
			f, err := s.lib.DeleteTrack(ctx, tid)
			if err != nil {
				s.libError(w, r, err)
				return
			}
			files = append(files, f...)
			deleted++
		}
		trashed, failed := s.trashFiles(r, files)
		out["deleted_tracks"], out["trashed"], out["trash_failed"] = int64(deleted), int64(trashed), int64(failed)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) restoreAlbum(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.lib.RestoreAlbum(r.Context(), id, s.importer.Original)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeGroup(w, g)
}

func (s *Server) artist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	a, err := s.lib.ArtistDetail(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if a == nil {
		writeError(w, http.StatusNotFound, library.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// editArtist renames an artist (every track and album using exactly that name) and/or sets its
// aliases; each is its own entry in the edit log.
func (s *Server) editArtist(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		Name    *string   `json:"name"`
		Aliases *[]string `json:"aliases"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var g int64
	if req.Aliases != nil {
		if g, err = s.lib.SetAliases(r.Context(), "artist", id, *req.Aliases); err != nil {
			s.libError(w, r, err)
			return
		}
	}
	if req.Name != nil {
		g2, err := s.lib.RenameArtist(r.Context(), id, *req.Name)
		if err != nil {
			s.libError(w, r, err)
			return
		}
		g = max(g, g2)
	}
	// The renamed artist may be a different (new or existing) artist now.
	to := id
	if req.Name != nil {
		if a, _ := s.lib.ArtistByName(r.Context(), *req.Name); a != 0 {
			to = a
		}
	}
	writeJSON(w, http.StatusOK, map[string]int64{"group": g, "artist_id": to})
}

func (s *Server) editGroups(w http.ResponseWriter, r *http.Request) {
	limit, _ := pageArgs(r)
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	list, err := s.lib.EditGroups(r.Context(), limit, before)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) editGroup(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.lib.EditGroup(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	if g == nil {
		writeError(w, http.StatusNotFound, library.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// undo reverts a group. 200 with the fields left alone (conflicts) when anything was reverted;
// 409 with the conflicts when nothing could be.
func (s *Server) undo(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	g, conflicts, err := s.lib.Undo(r.Context(), id)
	if errors.Is(err, library.ErrAlreadyUndone) {
		writeError(w, http.StatusConflict, err)
		return
	}
	if err != nil {
		s.libError(w, r, err)
		return
	}
	if conflicts == nil {
		conflicts = []library.Conflict{}
	}
	status := http.StatusOK
	if g == 0 {
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]any{"group": g, "conflicts": conflicts})
}

// ---- MusicBrainz identification (only on request; sends the album title, artist and catalog) ----

func (s *Server) identifyError(w http.ResponseWriter, err error) {
	if errors.Is(err, identify.ErrUnavailable) {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	upstreamError(w, err, 0)
}

func (s *Server) albumOr404(w http.ResponseWriter, r *http.Request) *library.AlbumDetail {
	id, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return nil
	}
	a, err := s.lib.Album(r.Context(), id)
	if err != nil {
		s.internal(w, r, err)
		return nil
	}
	if a == nil {
		writeError(w, http.StatusNotFound, library.ErrNotFound)
	}
	return a
}

// identifySearch looks for releases matching the album, or the title/artist/catalog given.
func (s *Server) identifySearch(w http.ResponseWriter, r *http.Request) {
	a := s.albumOr404(w, r)
	if a == nil {
		return
	}
	q := identify.Query{Title: a.Title, Artist: a.AlbumArtist, Catalog: a.Catalog}
	if v := r.URL.Query(); v.Has("title") || v.Has("catalog") {
		q = identify.Query{Title: v.Get("title"), Artist: v.Get("artist"), Catalog: v.Get("catalog")}
	}
	q.Year, q.Tracks = a.Date, len(a.Entries)
	list, err := s.mb.Search(r.Context(), q)
	if err != nil {
		s.identifyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": q, "candidates": list})
}

func (s *Server) identifyPropose(w http.ResponseWriter, r *http.Request) {
	a := s.albumOr404(w, r)
	if a == nil {
		return
	}
	p, err := s.mb.Propose(r.Context(), a, r.PathValue("release"))
	if err != nil {
		s.identifyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// identifyApply applies the picked changes of a proposal as one undoable action.
func (s *Server) identifyApply(w http.ResponseWriter, r *http.Request) {
	a := s.albumOr404(w, r)
	if a == nil {
		return
	}
	var req struct {
		Keys []string `json:"keys"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx := r.Context()
	release := r.PathValue("release")
	p, err := s.mb.Propose(ctx, a, release)
	if err != nil {
		s.identifyError(w, err)
		return
	}
	changes := p.Selected(req.Keys)
	if p.Cover && slices.Contains(req.Keys, identify.CoverKey) {
		data, err := s.mb.FrontCover(ctx, release)
		if err != nil {
			s.identifyError(w, err)
			return
		}
		cover, err := s.importer.StoreCover(ctx, data)
		if err != nil {
			upstreamError(w, err, 0)
			return
		}
		changes = append(changes, library.Change{Target: "album", ID: a.ID, Field: "cover_id", Value: library.Str(strconv.FormatInt(cover, 10))})
	}
	if len(changes) == 0 {
		writeGroup(w, 0)
		return
	}
	g, err := s.lib.ApplyChanges(ctx, library.SourceIdentify, fmt.Sprintf("套用 MusicBrainz 資料到「%s」", a.Title), changes)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeGroup(w, g)
}

// folderAlbums lists the folders whose standalone tracks would make an album (files imported
// before folders without album tags became albums).
func (s *Server) folderAlbums(w http.ResponseWriter, r *http.Request) {
	groups, err := s.importer.FolderGroups(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

// makeFolderAlbums makes the chosen folders' albums as one undoable action.
func (s *Server) makeFolderAlbums(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Folders []importer.FolderChoice `json:"folders"`
	}
	if err := readJSON(r, &req); err != nil || len(req.Folders) == 0 || len(req.Folders) > 500 {
		writeError(w, http.StatusBadRequest, errors.New("expected the folders to make albums of"))
		return
	}
	g, err := s.importer.MakeFolderAlbums(r.Context(), req.Folders)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeGroup(w, g)
}

// vgmdbBody is an album the client read from a VGMdb page the user pasted, and on apply the
// picked changes.
type vgmdbBody struct {
	Album identify.VGMdbAlbum `json:"album"`
	Keys  []string            `json:"keys"`
}

func (s *Server) vgmdbProposal(w http.ResponseWriter, r *http.Request) (*library.AlbumDetail, *vgmdbBody, *identify.Proposal) {
	a := s.albumOr404(w, r)
	if a == nil {
		return nil, nil, nil
	}
	var req vgmdbBody
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return nil, nil, nil
	}
	if err := req.Album.Check(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return nil, nil, nil
	}
	return a, &req, identify.ProposeVGMdb(a, &req.Album)
}

// vgmdbPropose compares an album read from VGMdb with an album of the library.
func (s *Server) vgmdbPropose(w http.ResponseWriter, r *http.Request) {
	if _, _, p := s.vgmdbProposal(w, r); p != nil {
		writeJSON(w, http.StatusOK, p)
	}
}

// vgmdbApply applies the picked changes as one undoable action; the cover is fetched from VGMdb's
// image host only then.
func (s *Server) vgmdbApply(w http.ResponseWriter, r *http.Request) {
	a, req, p := s.vgmdbProposal(w, r)
	if p == nil {
		return
	}
	ctx := r.Context()
	changes := p.Selected(req.Keys)
	if p.Cover && slices.Contains(req.Keys, identify.CoverKey) && s.mb != nil {
		data, err := s.mb.VGMdbCover(ctx, &req.Album)
		if err != nil {
			upstreamError(w, err, 0)
			return
		}
		cover, err := s.importer.StoreCover(ctx, data)
		if err != nil {
			upstreamError(w, err, 0)
			return
		}
		changes = append(changes, library.Change{Target: "album", ID: a.ID, Field: "cover_id", Value: library.Str(strconv.FormatInt(cover, 10))})
	}
	if len(changes) == 0 {
		writeGroup(w, 0)
		return
	}
	g, err := s.lib.ApplyChanges(ctx, library.SourceVGMdb, fmt.Sprintf("套用 VGMdb 資料到「%s」", a.Title), changes)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeGroup(w, g)
}
