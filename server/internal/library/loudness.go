package library

import (
	"context"
	"database/sql"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/HHim8826/kanade/server/internal/db"
)

// Loudness (review #136): each file's integrated loudness and sample peak, measured by FFmpeg, for
// the player's volume balance. An album's is worked out from its songs' whenever it is asked for, so
// it follows merges, splits and songs added or taken out.

// MethodEBUR128 is FFmpeg's ebur128 filter: EBU R128 integrated loudness, sample peak.
const MethodEBUR128 = "ebur128"

type Loudness struct {
	LUFS float64 `json:"lufs"`
	Peak float64 `json:"peak"` // dBFS
}

// AlbumLoudness is an album's: its measured songs' loudness taken together (by their power, each
// for as long as it lasts), the loudest peak, and how many of its songs were measured.
type AlbumLoudness struct {
	Loudness
	Measured int `json:"measured"`
	Songs    int `json:"songs"`
}

func (s *Store) SetLoudness(ctx context.Context, assetID int64, lufs, peak float64, method string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO loudness (asset_id, lufs, peak, method, error, measured_at) VALUES (?, ?, ?, ?, '', ?)
		ON CONFLICT (asset_id) DO UPDATE SET lufs = excluded.lufs, peak = excluded.peak, method = excluded.method, error = '',
		measured_at = excluded.measured_at`, assetID, lufs, peak, method, db.Now())
	return err
}

// LoudnessFailed records why a file could not be measured; a measurement it had stays.
func (s *Store) LoudnessFailed(ctx context.Context, assetID int64, why string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO loudness (asset_id, method, error, measured_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (asset_id) DO UPDATE SET error = excluded.error, measured_at = excluded.measured_at WHERE lufs IS NULL`,
		assetID, MethodEBUR128, why, db.Now())
	return err
}

// LoudnessKnown reports whether a file was measured, or tried and failed.
func (s *Store) LoudnessKnown(ctx context.Context, assetID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM loudness WHERE asset_id = ?`, assetID).Scan(&n)
	return n > 0, err
}

func idList(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

// AssetLoudness is the measured files among ids.
func (s *Store) AssetLoudness(ctx context.Context, ids []int64) (map[int64]Loudness, error) {
	out := map[int64]Loudness{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT asset_id, lufs, peak FROM loudness WHERE lufs IS NOT NULL AND asset_id IN (`+idList(ids)+`)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var l Loudness
		if err := rows.Scan(&id, &l.LUFS, &l.Peak); err != nil {
			return nil, err
		}
		out[id] = l
	}
	return out, rows.Err()
}

// AlbumLoudness is the albums among ids that have a measured song.
func (s *Store) AlbumLoudness(ctx context.Context, ids []int64) (map[int64]AlbumLoudness, error) {
	out := map[int64]AlbumLoudness{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.album_id, a.duration_ms, l.lufs, l.peak FROM album_entries e
		JOIN assets a ON a.id = e.asset_id LEFT JOIN loudness l ON l.asset_id = a.id AND l.lufs IS NOT NULL
		WHERE a.state = 'verified' AND e.album_id IN (`+idList(ids)+`)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type sum struct {
		power, ms float64
		peak      float64
		measured  int
		songs     int
	}
	sums := map[int64]*sum{}
	for rows.Next() {
		var album, ms int64
		var lufs, peak sql.NullFloat64
		if err := rows.Scan(&album, &ms, &lufs, &peak); err != nil {
			return nil, err
		}
		a := sums[album]
		if a == nil {
			a = &sum{peak: math.Inf(-1)}
			sums[album] = a
		}
		a.songs++
		if !lufs.Valid {
			continue
		}
		w := float64(max(ms, 1000))
		a.power += w * math.Pow(10, lufs.Float64/10)
		a.ms += w
		a.peak = max(a.peak, peak.Float64)
		a.measured++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id, a := range sums {
		if a.measured == 0 {
			continue
		}
		lufs := 10 * math.Log10(a.power/a.ms)
		out[id] = AlbumLoudness{Loudness: Loudness{LUFS: math.Round(lufs*10) / 10, Peak: a.peak}, Measured: a.measured, Songs: a.songs}
	}
	return out, nil
}

// LoudnessStatus counts the library's files: measured, failed, and all of them (verified ones).
type LoudnessStatus struct {
	Measured int      `json:"measured"`
	Failed   int      `json:"failed"`
	Files    int      `json:"files"`
	Pending  int64    `json:"pending_bytes"`    // the size of the files not measured or tried yet: what a scan reads from Drive
	Median   *float64 `json:"median,omitempty"` // the measured files' median loudness: what an unmeasured one is taken for
}

// LoudnessStatus counts; its median is LoudnessMedian's.
func (s *Store) LoudnessStatus(ctx context.Context) (*LoudnessStatus, error) {
	st := &LoudnessStatus{}
	err := s.db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(l.lufs IS NOT NULL), 0), coalesce(sum(l.lufs IS NULL AND l.asset_id IS NOT NULL), 0),
		coalesce(sum(CASE WHEN l.asset_id IS NULL THEN a.size END), 0)
		FROM assets a LEFT JOIN loudness l ON l.asset_id = a.id WHERE a.state = 'verified'`).Scan(&st.Files, &st.Measured, &st.Failed, &st.Pending)
	if err != nil {
		return nil, err
	}
	st.Median, err = s.LoudnessMedian(ctx)
	return st, err
}

// medianFor is how long a median is used before it is worked out again: it reads every measured
// file, and hardly moves (review #146).
const medianFor = time.Minute

// LoudnessMedian is the measured files' median loudness (nil while none is), worked out at most
// once a minute.
func (s *Store) LoudnessMedian(ctx context.Context) (*float64, error) {
	s.medianMu.Lock()
	defer s.medianMu.Unlock()
	if !s.medianAt.IsZero() && time.Since(s.medianAt) < medianFor {
		return s.median, nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM loudness l JOIN assets a ON a.id = l.asset_id
		WHERE a.state = 'verified' AND l.lufs IS NOT NULL`).Scan(&n); err != nil {
		return nil, err
	}
	var median *float64
	if n > 0 {
		// The middle one or two, in order: SQLite walks the sorted values without keeping them.
		rows, err := s.db.QueryContext(ctx, `SELECT l.lufs FROM loudness l JOIN assets a ON a.id = l.asset_id
			WHERE a.state = 'verified' AND l.lufs IS NOT NULL ORDER BY l.lufs LIMIT ? OFFSET ?`, 2-n%2, (n-1)/2)
		if err != nil {
			return nil, err
		}
		var sum float64
		k := 0
		for rows.Next() {
			var v float64
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return nil, err
			}
			sum += v
			k++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if k > 0 {
			m := math.Round(sum/float64(k)*10) / 10
			median = &m
		}
	}
	s.median, s.medianAt = median, time.Now()
	return median, nil
}

// Unmeasured is a file still to be measured.
type Unmeasured struct {
	AssetID     int64
	DriveFileID string
	Format      string
	Size        int64
}

// Unmeasured lists files never measured (and, with failed, those that could not be), at most limit,
// after the asset after (to go on from where a scan was).
func (s *Store) Unmeasured(ctx context.Context, after int64, limit int, failed bool) ([]Unmeasured, error) {
	cond := `l.asset_id IS NULL`
	if failed {
		cond = `(l.asset_id IS NULL OR l.lufs IS NULL)`
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.id, a.drive_file_id, a.format, a.size FROM assets a LEFT JOIN loudness l ON l.asset_id = a.id
		WHERE a.state = 'verified' AND a.drive_file_id IS NOT NULL AND a.id > ? AND `+cond+` ORDER BY a.id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Unmeasured
	for rows.Next() {
		var u Unmeasured
		if err := rows.Scan(&u.AssetID, &u.DriveFileID, &u.Format, &u.Size); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// AssetByDriveID is the verified file stored as a Drive file, 0 when none is.
func (s *Store) AssetByDriveID(ctx context.Context, driveID string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM assets WHERE drive_file_id = ? AND state = 'verified'`, driveID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}
