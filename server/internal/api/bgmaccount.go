package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/HHim8826/kanade/server/internal/bangumi"
	"github.com/HHim8826/kanade/server/internal/library"
)

// An album's own entry in Bangumi (a music subject), and its owner's collection of it through their
// linked Bangumi account (review #94): status, rating, tags, comment, private. Only what the owner
// asks to change is written to Bangumi; playing or browsing changes nothing there.

func (s *Server) bgmError(w http.ResponseWriter, r *http.Request, err error) {
	var refused *bangumi.ErrRefused
	switch {
	case errors.As(err, &refused):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, bangumi.ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, bangumi.ErrNotLinked):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "reason": "not_linked"})
	default:
		s.bangumiError(w, r, err)
	}
}

// bgmAccountInfo is the settings page's: the application and the link.
func (s *Server) bgmAccountInfo(w http.ResponseWriter, r *http.Request) {
	app := s.bgmAccounts.App(r.Context())
	link, err := s.bgmAccounts.Link(r.Context(), userID(r))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"app_id": app.ID, "has_secret": app.Secret != "", "redirect_uri": app.RedirectURI, "link": link})
}

func (s *Server) setBgmApp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"app_id"`
		Secret string `json:"app_secret"` // empty: kept
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.bgmAccounts.SetApp(r.Context(), req.ID, req.Secret); err != nil {
		s.bgmError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) beginBgmLink(w http.ResponseWriter, r *http.Request) {
	u, err := s.bgmAccounts.Begin(r.Context(), userID(r))
	if err != nil {
		s.bgmError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": u})
}

// bgmCallback is where Bangumi sends the person back; the state says whose link it is (the login
// cookie does not come along).
func (s *Server) bgmCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	back := func(result string) { http.Redirect(w, r, "/app/#/settings?bangumi="+result, http.StatusFound) }
	if q.Get("error") != "" || q.Get("code") == "" {
		back("denied")
		return
	}
	_, err := s.bgmAccounts.Finish(r.Context(), q.Get("state"), q.Get("code"))
	switch {
	case errors.Is(err, bangumi.ErrState):
		back("expired")
	case errors.Is(err, bangumi.ErrAuth):
		s.log.Warn("bangumi: link refused", "err", err)
		back("refused")
	case err != nil:
		s.log.Warn("bangumi: link failed", "err", err)
		back("failed")
	default:
		back("linked")
	}
}

func (s *Server) unlinkBgm(w http.ResponseWriter, r *http.Request) {
	if err := s.bgmAccounts.Unlink(r.Context(), userID(r)); err != nil {
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setAlbumSubject makes an album a Bangumi music subject ({source_id}: its number or link).
func (s *Server) setAlbumSubject(w http.ResponseWriter, r *http.Request) {
	album, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		SourceID string `json:"source_id"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sid, ok := bangumi.Ref(req.SourceID)
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("source_id is a Bangumi subject's number or link"))
		return
	}
	var work int64
	sub, err := s.bgm.Subject(r.Context(), sid)
	switch {
	case err == nil:
		if sub.Type != bangumi.Music {
			writeError(w, http.StatusBadRequest, errors.New("this subject is not music: link an anime or a game as the album's work instead"))
			return
		}
		if work, err = s.lib.PutWork(r.Context(), workData(sub)); err != nil {
			s.libError(w, r, err)
			return
		}
	case errors.Is(err, bangumi.ErrUnavailable):
		wk, err2 := s.lib.WorkFor(r.Context(), library.SourceBangumi, strconv.FormatInt(sid, 10))
		if err2 != nil || wk == nil {
			s.bangumiError(w, r, err)
			return
		}
		work = wk.ID
	default:
		s.bangumiError(w, r, err)
		return
	}
	g, err := s.lib.SetAlbumSubject(r.Context(), album, work)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": g, "work_id": work})
}

func (s *Server) clearAlbumSubject(w http.ResponseWriter, r *http.Request) {
	album, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	g, err := s.lib.SetAlbumSubject(r.Context(), album, 0)
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": g})
}

// subjectOf is the Bangumi subject an album is, by number; 0 with a 404 written when none.
func (s *Server) subjectOf(w http.ResponseWriter, r *http.Request) (*library.Work, int64) {
	album, err := pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return nil, 0
	}
	wk, err := s.lib.AlbumSubject(r.Context(), album)
	if err != nil {
		s.internal(w, r, err)
		return nil, 0
	}
	if wk == nil {
		writeError(w, http.StatusNotFound, errors.New("the album has no Bangumi subject"))
		return nil, 0
	}
	sid, _ := strconv.ParseInt(wk.SourceID, 10, 64)
	return wk, sid
}

// albumCollection is the album's subject and how its owner collected it: with the tags people put
// on the subject and the owner's own, to pick from.
func (s *Server) albumCollection(w http.ResponseWriter, r *http.Request) {
	wk, sid := s.subjectOf(w, r)
	if wk == nil {
		return
	}
	out := map[string]any{"subject": wk, "url": s.bgm.URL(sid), "linked": false, "subject_tags": []string{}, "my_tags": []string{}}
	if sub, err := s.bgm.Subject(r.Context(), sid); err == nil {
		tags := []string{}
		for _, t := range sub.Tags {
			if len(tags) < 15 && !strings.ContainsAny(t.Name, " \t") {
				tags = append(tags, t.Name)
			}
		}
		out["subject_tags"] = tags
	}
	sess, err := s.bgmAccounts.Session(r.Context(), userID(r))
	if errors.Is(err, bangumi.ErrNotLinked) {
		writeJSON(w, http.StatusOK, out)
		return
	}
	if err != nil {
		s.bgmError(w, r, err)
		return
	}
	out["linked"], out["username"] = true, sess.Username
	col, err := s.bgm.Collection(r.Context(), sess.Access, sess.Username, sid)
	if errors.Is(err, bangumi.ErrAuth) {
		s.bgmAccounts.Failed(userID(r))
		s.bgmError(w, r, bangumi.ErrNotLinked)
		return
	}
	if err != nil {
		s.bgmError(w, r, err)
		return
	}
	if col != nil {
		col.Subject = nil
	}
	out["collection"] = col
	if tags, err := s.bgmAccounts.MyTags(r.Context(), userID(r), sess); err == nil {
		out["my_tags"] = tags
	}
	writeJSON(w, http.StatusOK, out)
}

// setAlbumCollection writes the owner's collection of the album's subject to Bangumi: {type (1 to
// 5), rate (0 to 10, 0 none), comment, private, tags}.
func (s *Server) setAlbumCollection(w http.ResponseWriter, r *http.Request) {
	wk, sid := s.subjectOf(w, r)
	if wk == nil {
		return
	}
	var req struct {
		Type    int      `json:"type"`
		Rate    int      `json:"rate"`
		Comment string   `json:"comment"`
		Private bool     `json:"private"`
		Tags    []string `json:"tags"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	tags := []string{}
	for _, t := range req.Tags {
		if t = strings.TrimSpace(t); t != "" && !contains(tags, t) {
			tags = append(tags, t)
		}
	}
	switch {
	case req.Type < bangumi.Wish || req.Type > bangumi.Dropped:
		writeError(w, http.StatusBadRequest, errors.New("type is from 1 to 5"))
		return
	case req.Rate < 0 || req.Rate > 10:
		writeError(w, http.StatusBadRequest, errors.New("rate is from 0 to 10"))
		return
	case len(tags) > 10:
		writeError(w, http.StatusBadRequest, errors.New("at most 10 tags"))
		return
	case utf8.RuneCountInString(req.Comment) > 1000:
		writeError(w, http.StatusBadRequest, errors.New("the comment is too long"))
		return
	}
	for _, t := range tags {
		if strings.ContainsAny(t, " \t\n") || utf8.RuneCountInString(t) > 30 {
			writeError(w, http.StatusBadRequest, errors.New("a tag has no spaces and is at most 30 characters"))
			return
		}
	}
	sess, err := s.bgmAccounts.Session(r.Context(), userID(r))
	if err != nil {
		s.bgmError(w, r, err)
		return
	}
	comment := strings.TrimSpace(req.Comment)
	err = s.bgm.SetCollection(r.Context(), sess.Access, sid, bangumi.CollectionChange{Type: &req.Type, Rate: &req.Rate, Comment: &comment,
		Private: &req.Private, Tags: &tags})
	if errors.Is(err, bangumi.ErrAuth) {
		s.bgmAccounts.Failed(userID(r))
		err = bangumi.ErrNotLinked
	}
	if err != nil {
		s.bgmError(w, r, err)
		return
	}
	s.bgmAccounts.Changed(userID(r))
	w.WriteHeader(http.StatusNoContent)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// bgmCollections is a page of the owner's music collections in Bangumi (?type: 1 to 5, else all;
// ?offset), each with the library's albums that are that subject.
func (s *Server) bgmCollections(w http.ResponseWriter, r *http.Request) {
	typ, _ := strconv.Atoi(r.URL.Query().Get("type"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	sess, err := s.bgmAccounts.Session(r.Context(), userID(r))
	if err != nil {
		s.bgmError(w, r, err)
		return
	}
	const limit = 30
	p, err := s.bgm.Collections(r.Context(), sess.Access, sess.Username, bangumi.Music, typ, limit, max(offset, 0))
	if errors.Is(err, bangumi.ErrAuth) {
		s.bgmAccounts.Failed(userID(r))
		err = bangumi.ErrNotLinked
	}
	if err != nil {
		s.bgmError(w, r, err)
		return
	}
	ids := make([]string, 0, len(p.Data))
	for _, c := range p.Data {
		ids = append(ids, strconv.FormatInt(c.SubjectID, 10))
	}
	albums, err := s.lib.AlbumsAsSubjects(r.Context(), ids)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	type item struct {
		bangumi.Collection
		Name   string                 `json:"name"`
		NameCN string                 `json:"name_cn,omitempty"`
		Date   string                 `json:"date,omitempty"`
		Score  float64                `json:"score,omitempty"`
		Image  bool                   `json:"image"`
		URL    string                 `json:"url"`
		Albums []library.AlbumSummary `json:"albums"`
	}
	out := []item{}
	for _, c := range p.Data {
		it := item{Collection: c, URL: s.bgm.URL(c.SubjectID), Albums: albums[strconv.FormatInt(c.SubjectID, 10)]}
		if c.Subject != nil {
			it.Name, it.NameCN, it.Date, it.Score, it.Image = c.Subject.Name, c.Subject.NameCN, c.Subject.Date, c.Subject.Score, c.Subject.Image() != ""
		}
		if it.Albums == nil {
			it.Albums = []library.AlbumSummary{}
		}
		it.Subject = nil
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": p.Total, "offset": offset, "page_size": limit, "username": sess.Username, "items": out})
}
