package importer

import (
	"context"
	"path/filepath"
	"testing"
)

// A file's composer, lyricist and arranger come in with it; songs imported before they were kept
// get them from the fields read then, once (D13).
func TestCreditsImported(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	if err := im.BackfillCredits(ctx); err != nil { // at start, as on a new library
		t.Fatal(err)
	}
	src := t.TempDir()
	taggedMP3(t, filepath.Join(src, "Single", "01.mp3"), map[string]string{"TIT2": "Song", "TPE1": "Singer", "TALB": "Single",
		"TCOM": "ryo", "TEXT": "kz", "TXXX": "ARRANGER\x00livetune"})
	batch, _, err := im.CreateBatch(ctx, "local", "", src, false)
	if err != nil {
		t.Fatal(err)
	}
	runUntilDone(t, im, batch)
	var id int64
	im.db.QueryRow(`SELECT id FROM tracks`).Scan(&id)
	d, err := lib.Track(ctx, id)
	if err != nil || d == nil || d.Composer != "ryo" || d.Lyricist != "kz" || d.Arranger != "livetune" || len(d.Credits) != 4 {
		t.Fatalf("imported %+v %v", d, err)
	}

	im.db.Exec(`UPDATE tracks SET composer = '', lyricist = '', arranger = ''`)
	im.db.Exec(`DELETE FROM credits WHERE role != 'artist'`)
	if err := im.BackfillCredits(ctx); err != nil {
		t.Fatal(err)
	}
	if d, _ = lib.Track(ctx, id); d.Composer != "" {
		t.Fatalf("filled again after it ran: %+v", d)
	}
	im.db.Exec(`DELETE FROM settings WHERE key = 'credit_names'`)
	if err := im.BackfillCredits(ctx); err != nil {
		t.Fatal(err)
	}
	if d, _ = lib.Track(ctx, id); d.Composer != "ryo" || d.Lyricist != "kz" || d.Arranger != "livetune" || len(d.Credits) != 4 {
		t.Fatalf("filled %+v", d)
	}
}
