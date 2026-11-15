package importer

import (
	"context"
	"encoding/json"

	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/media"
)

// BackfillCredits gives the songs imported before Kanade kept composers, lyricists and arrangers
// those the files named, from the fields read at import (decision D13): the latest file of each
// song tells. Once.
func (im *Importer) BackfillCredits(ctx context.Context) error {
	if im.lib.RoleNamesFilled(ctx) {
		return nil
	}
	rows, err := im.db.QueryContext(ctx, `SELECT track_id, info FROM import_items
		WHERE track_id IS NOT NULL AND info IS NOT NULL AND role = 'audio' ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	names := map[int64]library.RoleNames{}
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		var info media.Info
		if json.Unmarshal(raw, &info) != nil {
			continue
		}
		info.FillCredits()
		names[id] = library.RoleNames{Composer: info.Tags.Composer, Lyricist: info.Tags.Lyricist, Arranger: info.Tags.Arranger}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	return im.lib.FillRoleNames(ctx, names)
}
