package api

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/HHim8826/kanade/server/internal/library"
)

// Personal listening statistics (review #93). Every request names the time zone days are counted
// in (tz, an IANA name such as Asia/Taipei; the page sends the browser's or the one chosen); kind is
// music, spoken or all.

func statsZone(r *http.Request) (*time.Location, error) {
	tz := r.URL.Query().Get("tz")
	if tz == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, errors.New("unknown time zone")
	}
	return loc, nil
}

func statsKind(r *http.Request) string {
	if k := r.URL.Query().Get("kind"); k == "music" || k == "spoken" {
		return k
	}
	return ""
}

// statsRange reads from and to (YYYY-MM-DD, both days included) as Unix ms, from inclusive and to
// exclusive; neither given is all time.
func statsRange(r *http.Request, loc *time.Location) (int64, int64, error) {
	q := r.URL.Query()
	from, to := int64(0), time.Now().Add(24*time.Hour).UnixMilli()
	if v := q.Get("from"); v != "" {
		d, err := time.ParseInLocation(time.DateOnly, v, loc)
		if err != nil {
			return 0, 0, errors.New("from is YYYY-MM-DD")
		}
		from = d.UnixMilli()
	}
	if v := q.Get("to"); v != "" {
		d, err := time.ParseInLocation(time.DateOnly, v, loc)
		if err != nil {
			return 0, 0, errors.New("to is YYYY-MM-DD")
		}
		to = d.AddDate(0, 0, 1).UnixMilli()
	}
	if to <= from {
		return 0, 0, errors.New("to is before from")
	}
	return from, to, nil
}

// statsDays is a year of days for the heat map.
func (s *Server) statsDays(w http.ResponseWriter, r *http.Request) {
	loc, err := statsZone(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	year, err := strconv.Atoi(r.URL.Query().Get("year"))
	if err != nil || year < 1970 || year > 9999 {
		year = time.Now().In(loc).Year()
	}
	from, to := time.Date(year, 1, 1, 0, 0, 0, 0, loc), time.Date(year+1, 1, 1, 0, 0, 0, 0, loc)
	days, err := s.lib.ListeningDays(r.Context(), loc, from.UnixMilli(), to.UnixMilli(), statsKind(r))
	if err != nil {
		s.internal(w, r, err)
		return
	}
	until, err := s.lib.EstimatedUntil(r.Context())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	estimated := ""
	if until > 0 {
		estimated = time.UnixMilli(until).In(loc).Format(time.DateOnly)
	}
	writeJSON(w, http.StatusOK, map[string]any{"year": year, "days": days, "estimated_until": estimated})
}

func (s *Server) statsDay(w http.ResponseWriter, r *http.Request) {
	loc, err := statsZone(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	d, err := s.lib.ListeningDay(r.Context(), loc, r.URL.Query().Get("date"), statsKind(r))
	if err != nil {
		s.libError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// statsSummary adds up today, this week (from Monday), this month and this year, and the range
// asked for when there is one.
func (s *Server) statsSummary(w http.ResponseWriter, r *http.Request) {
	loc, err := statsZone(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	week := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	year := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, loc)
	end := today.AddDate(0, 0, 1).UnixMilli()
	out := map[string]*library.Summary{}
	for k, from := range map[string]time.Time{"today": today, "week": week, "month": month, "year": year} {
		if out[k], err = s.lib.ListeningSummary(r.Context(), loc, from.UnixMilli(), end, statsKind(r)); err != nil {
			s.internal(w, r, err)
			return
		}
	}
	if r.URL.Query().Get("from") != "" || r.URL.Query().Get("to") != "" {
		from, to, err := statsRange(r, loc)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if out["range"], err = s.lib.ListeningSummary(r.Context(), loc, from, to, statsKind(r)); err != nil {
			s.internal(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) statsTop(w http.ResponseWriter, r *http.Request) {
	loc, err := statsZone(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	from, to, err := statsRange(r, loc)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	list, err := s.lib.ListeningTop(r.Context(), from, to, statsKind(r), q.Get("group"), q.Get("by"), limit)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) statsTrends(w http.ResponseWriter, r *http.Request) {
	loc, err := statsZone(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	q := r.URL.Query()
	if q.Get("from") == "" || q.Get("to") == "" {
		writeError(w, http.StatusBadRequest, errors.New("trends need from and to"))
		return
	}
	from, to, err := statsRange(r, loc)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if to-from > 400*24*3600*1000 {
		writeError(w, http.StatusBadRequest, errors.New("trends cover at most 400 days"))
		return
	}
	tr, err := s.lib.ListeningTrends(r.Context(), loc, from, to, statsKind(r), time.Now())
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, tr)
}

// statsExport gives every span of listening as CSV or JSON, to keep or to work with elsewhere.
func (s *Server) statsExport(w http.ResponseWriter, r *http.Request) {
	loc, err := statsZone(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name := "kanade-listening-" + time.Now().In(loc).Format("20060102")
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.json"`)
		w.Write([]byte("["))
		first := true
		err = s.lib.ExportListening(r.Context(), loc, func(row library.ListenedRow) error {
			b, _ := json.Marshal(row)
			if !first {
				w.Write([]byte(",\n"))
			}
			first = false
			_, err := w.Write(b)
			return err
		})
		w.Write([]byte("]\n"))
	} else {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
		w.Write([]byte("\ufeff")) // spreadsheets read UTF-8 with the mark
		c := csv.NewWriter(w)
		c.Write([]string{"start", "track_id", "title", "artist", "album", "kind", "ms", "counted", "estimated"})
		err = s.lib.ExportListening(r.Context(), loc, func(row library.ListenedRow) error {
			return c.Write([]string{row.Start, strconv.FormatInt(row.TrackID, 10), row.Title, row.Artist, row.Album, row.Kind,
				strconv.FormatInt(row.MS, 10), strconv.FormatBool(row.Counted), strconv.FormatBool(row.Estimated)})
		})
		c.Flush()
	}
	if err != nil {
		s.log.Warn("export listening", "err", err)
	}
}

func (s *Server) statsClear(w http.ResponseWriter, r *http.Request) {
	if err := s.lib.ClearListening(r.Context()); err != nil {
		s.internal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
