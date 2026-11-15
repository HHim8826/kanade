package library

import (
	"context"
	"testing"
)

func TestVariant(t *testing.T) {
	cases := map[string]string{
		"only my railgun (instrumental)": VariantInstrumental, "Hikaru Nara -Instrumental-": VariantInstrumental,
		"Song (Inst.)": VariantInstrumental, "Song inst": VariantInstrumental, "Song (Off Vocal)": VariantInstrumental,
		"Song (off vocal ver.)": VariantInstrumental, "Song -off vo.-": VariantInstrumental, "Song (offvocal)": VariantInstrumental,
		"Song (Karaoke)": VariantInstrumental, "Song (without vocals)": VariantInstrumental, "Song (Backing Track)": VariantInstrumental,
		"恋愛サーキュレーション（オリジナル・カラオケ）": VariantInstrumental, "曲 (オフボーカル)": VariantInstrumental,
		"曲 (インストゥルメンタル)": VariantInstrumental, "歌 (伴奏)": VariantInstrumental, "ＳＯＮＧ （ＩＮＳＴＲＵＭＥＮＴＡＬ）": VariantInstrumental,
		"Song (TV size)": VariantShort, "Song (TV-Size)": VariantShort, "Song (TVsize)": VariantShort, "Song -TV ver.-": VariantShort,
		"Song (TV Version)": VariantShort, "Song (TV Edit)": VariantShort, "Song (Short Ver.)": VariantShort,
		"Song (short size)": VariantShort, "曲 (TVサイズ)": VariantShort, "曲（ＴＶサイズ）": VariantShort, "Song (Movie Size)": VariantShort,
		"曲 (ショートバージョン)": VariantShort, "Song (Game Size)": VariantShort,
		"Instinct": "", "Installation": "", "Short Story": "", "Turn off the light": "", "Off the voice": "",
		"Karaokee": "", "TVアニメ主題歌": "", "Television": "", "Shortcake": "", "ショート": "", "": "",
	}
	for title, want := range cases {
		if got := Variant(title, ""); got != want {
			t.Errorf("%q: %q, want %q", title, got, want)
		}
	}
	if Variant("Song", "Off vocal") != VariantInstrumental || Variant("Song", "Live") != "" {
		t.Error("the version note is not read")
	}
}

// A song's variant is told when it is published, kept up as its title or note changes, and for
// songs from before variants at start; shuffling plain leaves such songs out unless they are all
// there is.
func TestVariantsKeptAndShuffled(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	vocal, _ := s.Publish(ctx, verifiedAsset(t, s, "v1"), EntryInput{Title: "Song", Artist: "A", Album: "Single"})
	inst, _ := s.Publish(ctx, verifiedAsset(t, s, "v2"), EntryInput{Title: "Song (Instrumental)", Artist: "A", Album: "Single"})
	tv, _ := s.Publish(ctx, verifiedAsset(t, s, "v3"), EntryInput{Title: "Song", Artist: "A", Album: "Single"})
	variant := func(id int64) string {
		var v string
		s.db.QueryRow(`SELECT variant FROM tracks WHERE id = ?`, id).Scan(&v)
		return v
	}
	if variant(vocal.TrackID) != "" || variant(inst.TrackID) != VariantInstrumental {
		t.Fatalf("published: %q %q", variant(vocal.TrackID), variant(inst.TrackID))
	}
	if _, err := s.ApplyChanges(ctx, SourceUser, "note", []Change{{"track", tv.TrackID, "version", Str("TV size")}}); err != nil {
		t.Fatal(err)
	}
	if variant(tv.TrackID) != VariantShort {
		t.Fatalf("after the note: %q", variant(tv.TrackID))
	}
	g, err := s.ApplyChanges(ctx, SourceUser, "rename", []Change{{"track", inst.TrackID, "title", Str("Song (Live)")}})
	if err != nil || variant(inst.TrackID) != "" {
		t.Fatalf("after the rename: %q %v", variant(inst.TrackID), err)
	}
	if _, _, err := s.Undo(ctx, g); err != nil || variant(inst.TrackID) != VariantInstrumental {
		t.Fatalf("after undoing it: %q %v", variant(inst.TrackID), err)
	}
	for range 20 {
		list, err := s.RandomTracks(ctx, 5, "music", true, nil)
		if err != nil || len(list) != 1 || list[0].ID != vocal.TrackID {
			t.Fatalf("plain shuffle: %+v %v", list, err)
		}
	}
	if list, _ := s.RandomTracks(ctx, 5, "music", true, []int64{vocal.TrackID}); len(list) != 1 || list[0].ID != vocal.TrackID {
		t.Fatalf("plain shuffle after the only plain song: %+v", list) // rounds again rather than play a variant
	}
	s.db.Exec(`UPDATE tracks SET variant = '' WHERE id = ?`, vocal.TrackID)
	s.db.Exec(`UPDATE tracks SET variant = 'instrumental' WHERE id = ?`, vocal.TrackID)
	if list, _ := s.RandomTracks(ctx, 5, "music", true, nil); len(list) != 3 {
		t.Fatalf("all variants: %d", len(list))
	}
	// Songs from before variants are told at start.
	s.db.Exec(`UPDATE tracks SET variant = ''`)
	s.db.Exec(`DELETE FROM settings WHERE key = 'variants'`)
	if err := s.EnsureVariants(ctx); err != nil {
		t.Fatal(err)
	}
	if variant(vocal.TrackID) != "" || variant(inst.TrackID) != VariantInstrumental || variant(tv.TrackID) != VariantShort {
		t.Fatalf("at start: %q %q %q", variant(vocal.TrackID), variant(inst.TrackID), variant(tv.TrackID))
	}
	var albumID int64
	s.db.QueryRow(`SELECT album_id FROM album_entries WHERE id = ?`, vocal.EntryID).Scan(&albumID)
	d, _ := s.Album(ctx, albumID)
	n := 0
	for _, e := range d.Entries {
		if e.Variant != "" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("the album says %d variants", n)
	}
}
