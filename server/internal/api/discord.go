package api

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/HHim8826/kanade/server/internal/discord"
	"github.com/HHim8826/kanade/server/internal/presence"
)

// Discord's status (review #135): web players tell what they play; the server shows it as the
// linked person's Discord status. Linking is Discord's OAuth, from the settings page.

var playerID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// presenceInfo says whether this account's players tell what they play (it is linked).
func (s *Server) presenceInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"publish": s.discord.Linked(r.Context(), userID(r))})
}

// reportPlayer takes what a web player plays; publish says whether to go on telling. Nothing is
// kept while the account is not linked.
func (s *Server) reportPlayer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("pid")
	if !playerID.MatchString(id) {
		writeError(w, http.StatusBadRequest, errors.New("bad player"))
		return
	}
	var rep presence.Report
	if err := readJSON(r, &rep); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	linked := s.discord.Linked(r.Context(), userID(r))
	if linked {
		s.presence.Put(userID(r), sessionID(r), id, rep)
	} else {
		s.presence.Remove(userID(r), id)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"publish": linked})
}

func (s *Server) removePlayer(w http.ResponseWriter, r *http.Request) {
	s.presence.Remove(userID(r), r.PathValue("pid"))
	w.WriteHeader(http.StatusNoContent)
}

// discordInfo is the settings page's: the application, the link and how it is doing, and the
// browsers that play now (to follow one).
func (s *Server) discordInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	app := s.discord.App(ctx)
	link, err := s.discord.Link(ctx, userID(r))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"client_id": app.ClientID, "has_secret": app.Secret != "", "image": s.discord.Image(ctx),
		"redirect_uri": app.RedirectURI, "link": link, "state": s.discord.StateOf(userID(r)), "players": s.presence.Players(userID(r))})
}

func (s *Server) setDiscordApp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientID string `json:"client_id"`
		Secret   string `json:"client_secret"` // empty: kept
		Image    string `json:"image"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.discord.SetApp(r.Context(), req.ClientID, req.Secret, req.Image); err != nil {
		s.discordError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// beginDiscordLink is where to send the person to agree to the link.
func (s *Server) beginDiscordLink(w http.ResponseWriter, r *http.Request) {
	u, err := s.discord.Begin(r.Context(), userID(r))
	if err != nil {
		s.discordError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": u})
}

// discordCallback is where Discord sends the person back; the settings page then says how it went.
// The login cookie does not come along (SameSite=Strict): the state says whose link it is.
func (s *Server) discordCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	back := func(result string) {
		http.Redirect(w, r, "/app/#/settings?discord="+result, http.StatusFound)
	}
	if q.Get("error") != "" {
		back("denied")
		return
	}
	_, err := s.discord.Finish(r.Context(), q.Get("state"), q.Get("code"))
	switch {
	case errors.Is(err, discord.ErrState):
		back("expired")
	case errors.Is(err, discord.ErrScope):
		back("scope")
	case errors.Is(err, discord.ErrAuth):
		s.log.Warn("discord: link refused", "err", err)
		back("refused")
	case err != nil:
		s.log.Warn("discord: link failed", "err", err)
		back("failed")
	default:
		back("linked")
	}
}

func (s *Server) changeDiscordLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Follow     *string        `json:"follow"`
		FollowName *string        `json:"follow_name"`
		Show       *presence.Show `json:"show"`
		Status     *string        `json:"status"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.discord.Change(r.Context(), userID(r), req.Follow, req.FollowName, req.Show, req.Status); err != nil {
		s.discordError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) unlinkDiscord(w http.ResponseWriter, r *http.Request) {
	if err := s.discord.Unlink(r.Context(), userID(r)); err != nil {
		s.internal(w, r, err)
		return
	}
	s.presence.EndSessions(userID(r), -1) // nothing to tell any more
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) discordError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, discord.ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, discord.ErrNotLinked):
		writeError(w, http.StatusNotFound, err)
	default:
		s.internal(w, r, err)
	}
}
