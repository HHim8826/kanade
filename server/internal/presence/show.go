package presence

// Show is what is shown of the song playing elsewhere (Discord's status): its title always; the
// rest as chosen.
type Show struct {
	Artist bool   `json:"artist"`
	Album  bool   `json:"album"`
	Time   bool   `json:"time"`   // where in the song, as Discord's progress bar
	Paused string `json:"paused"` // "show": say it is paused; "clear": show nothing
	// Cover is the album's picture: "bangumi", its Bangumi entry's (public already); "all", else
	// Kanade's own through a public address made for it; "none", no picture.
	Cover string `json:"cover"`
}

// DefaultShow is what is shown until chosen otherwise.
var DefaultShow = Show{Artist: true, Album: true, Time: true, Paused: "clear", Cover: "bangumi"}

// Covers are the ways of Show.Cover.
var Covers = []string{"none", "bangumi", "all"}

// Shown is what is shown of the player followed, as Show says.
type Shown struct {
	State      string `json:"state"` // playing, paused or none
	Title      string `json:"title,omitempty"`
	Artist     string `json:"artist,omitempty"`
	Album      string `json:"album,omitempty"`
	PositionMS int64  `json:"position_ms,omitempty"` // with time shown: where in the song now
	DurationMS int64  `json:"duration_ms,omitempty"`
	AlbumID    int64  `json:"album_id,omitempty"` // with a cover shown: whose
}

// ShownBy is what show leaves of n (nil: nothing playing).
func ShownBy(n *Now, show Show) Shown {
	if n == nil || (n.State == Paused && show.Paused != "show") || n.Title == "" {
		return Shown{State: "none"}
	}
	s := Shown{State: n.State, Title: n.Title}
	if show.Artist {
		s.Artist = n.Artist
	}
	if show.Album {
		s.Album = n.Album
	}
	if show.Time && n.State == Playing && n.DurationMS > 0 {
		s.PositionMS, s.DurationMS = n.PositionMS, n.DurationMS
	}
	if show.Cover == "bangumi" || show.Cover == "all" {
		s.AlbumID = n.AlbumID
	}
	return s
}
