package library

import (
	"context"
	"fmt"
	"testing"
)

func TestSplit(t *testing.T) {
	cases := map[string]string{
		"A":                  "[{A }]",
		"A & B":              "[{A } {B }]",
		"A、B、C":              "[{A } {B } {C }]",
		"A / B":              "[{A } {B }]",
		"A/B":                "[{A } {B }]",
		"A feat. B":          "[{A } {B }]",
		"A ft. B & C":        "[{A } {B } {C }]",
		"A (feat. B)":        "[{A } {B }]",
		"A x B":              "[{A } {B }]",
		"A×B":                "[{A } {B }]",
		"A vs. B":            "[{A } {B }]",
		"Xmas":               "[{Xmas }]", // an x in a word
		"Earth, Wind & Fire": "[{Earth } {Wind } {Fire }]",
		"ミク (CV: 藤田咲)":       "[{ミク 藤田咲}]",
		"ミク（CV：藤田咲）、リン（CV：下田麻美）":     "[{ミク 藤田咲} {リン 下田麻美}]",
		"放課後ティータイム [CV.豊崎愛生 & 日笠陽子]": "[{放課後ティータイム 豊崎愛生 & 日笠陽子}]", // inside brackets, not divided
		"A & A":            "[{A }]",
		"A, B, etc.":       "[{A } {B }]", // a list's end is no one
		"A、B、他":            "[{A } {B }]",
		"NYO / Vocal:Kana": "[{NYO } {Kana }]",
		"Etcetera & B":     "[{Etcetera } {B }]",
		"":                 "[]",
	}
	for in, want := range cases {
		got := Split(in)
		if s := fmt.Sprint(got); s != want && !(want == "[]" && got == nil) {
			t.Errorf("%q: %s, want %s", in, s, want)
		}
	}
}

// Names a song is credited to are artists of their own once the user says how a string divides;
// until then the string is one artist. The word holds for songs imported later, renames follow it,
// and taking it back undoes it (D13).
func TestCredits(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	duet := "ミク (CV: 藤田咲) & リン (CV: 下田麻美)"
	one, err := s.Publish(ctx, verifiedAsset(t, s, "c1"), EntryInput{Title: "Song", Artist: duet, Album: "Single", Composer: "ryo", Lyricist: "ryo"})
	if err != nil {
		t.Fatal(err)
	}
	credits := func(id int64) string {
		d, err := s.Track(ctx, id)
		if err != nil || d == nil {
			t.Fatalf("track %d: %v", id, err)
		}
		out := ""
		for _, c := range d.Credits {
			out += fmt.Sprintf("%s:%s", c.Role, c.Name)
			if c.CreditedAs != "" {
				out += "@" + c.CreditedAs
			}
			out += " "
		}
		return out
	}
	if got := credits(one.TrackID); got != "artist:"+duet+" composer:ryo lyricist:ryo " {
		t.Fatalf("before a word: %s", got)
	}
	proposals, decided, err := s.Splits(ctx)
	if err != nil || len(proposals) != 1 || len(decided) != 0 || proposals[0].Name != duet || len(proposals[0].Parts) != 2 ||
		proposals[0].Parts[1].CV != "下田麻美" {
		t.Fatalf("proposals %+v decided %+v %v", proposals, decided, err)
	}
	parts := proposals[0].Parts
	g, err := s.DecideSplits(ctx, []SplitDecision{{Name: duet, Parts: &parts}})
	if err != nil {
		t.Fatal(err)
	}
	const divided = "artist:ミク artist:リン voice:藤田咲@ミク voice:下田麻美@リン composer:ryo lyricist:ryo "
	if got := credits(one.TrackID); got != divided {
		t.Fatalf("divided: %s", got)
	}
	if proposals, decided, _ = s.Splits(ctx); len(proposals) != 0 || len(decided) != 1 {
		t.Fatalf("after the word: %+v %+v", proposals, decided)
	}
	// A song imported later with the same string is divided as said.
	two, _ := s.Publish(ctx, verifiedAsset(t, s, "c2"), EntryInput{Title: "Other", Artist: duet, Album: "Single"})
	if got := credits(two.TrackID); got != "artist:ミク artist:リン voice:藤田咲@ミク voice:下田麻美@リン " {
		t.Fatalf("imported later: %s", got)
	}
	// The pages: who voiced whom, and the songs of each role.
	byName := map[string]Artist{}
	list, _ := s.Artists(ctx, 50, 0)
	for _, a := range list {
		byName[a.Name] = a
	}
	if len(list) != 4 || byName["ミク"].Kind != KindCharacter || byName["藤田咲"].Kind != KindPerson || byName["ミク"].Tracks != 2 {
		t.Fatalf("artists %+v", list) // ryo composed and wrote, sang nothing; the whole string is no one now
	}
	miku, _ := s.ArtistDetail(ctx, byName["ミク"].ID)
	if len(miku.Items) != 2 || len(miku.Voices) != 1 || miku.Voices[0].Name != "藤田咲" || len(miku.Roles) != 0 {
		t.Fatalf("character page %+v", miku)
	}
	fujita, _ := s.ArtistDetail(ctx, byName["藤田咲"].ID)
	if len(fujita.Items) != 0 || len(fujita.Roles) != 1 || fujita.Roles[0].Role != RoleVoice || len(fujita.Roles[0].Items) != 2 ||
		fujita.Roles[0].Items[0].As != "ミク" || len(fujita.Characters) != 1 || fujita.Characters[0].Name != "ミク" {
		t.Fatalf("voice page %+v", fujita)
	}
	res, _ := s.Search(ctx, "ryo", 10)
	if len(res.Artists) != 1 {
		t.Fatalf("search ryo: %+v", res.Artists)
	}
	ryo, _ := s.ArtistDetail(ctx, res.Artists[0].ID)
	if len(ryo.Roles) != 2 || ryo.Roles[0].Role != RoleComposer || ryo.Roles[1].Role != RoleLyricist {
		t.Fatalf("composer page %+v", ryo)
	}
	// A rename follows the word on how names divide.
	rename, err := s.RenameArtist(ctx, byName["藤田咲"].ID, "Fujita Saki")
	if err != nil {
		t.Fatal(err)
	}
	if got := credits(two.TrackID); got != "artist:ミク artist:リン voice:Fujita Saki@ミク voice:下田麻美@リン " {
		t.Fatalf("renamed: %s", got)
	}
	// Each role is a field to edit and undo.
	edit, err := s.ApplyChanges(ctx, SourceUser, "arranger", []Change{{"track", two.TrackID, "arranger", Str("kz、ryo")}})
	if err != nil {
		t.Fatal(err)
	}
	if got := credits(two.TrackID); got != "artist:ミク artist:リン voice:Fujita Saki@ミク voice:下田麻美@リン arranger:kz、ryo " {
		t.Fatalf("arranger: %s", got)
	}
	if _, _, err := s.Undo(ctx, edit); err != nil {
		t.Fatal(err)
	}
	if got := credits(two.TrackID); got != "artist:ミク artist:リン voice:Fujita Saki@ミク voice:下田麻美@リン " {
		t.Fatalf("arranger undone: %s", got)
	}
	// The word is not taken back over the rename that changed it since; once the rename is undone,
	// it is, and the string is whole again.
	if _, conflicts, err := s.Undo(ctx, g); err != nil || len(conflicts) != 1 || credits(one.TrackID) == "artist:"+duet+" composer:ryo lyricist:ryo " {
		t.Fatalf("undone over the rename: %+v %v", conflicts, err)
	}
	if _, conflicts, err := s.Undo(ctx, rename); err != nil || len(conflicts) != 0 {
		t.Fatalf("rename undone: %+v %v", conflicts, err)
	}
	if got := credits(one.TrackID); got != divided {
		t.Fatalf("rename undone: %s", got)
	}
	if _, conflicts, err := s.Undo(ctx, g); err != nil || len(conflicts) != 0 {
		t.Fatalf("word undone: %+v %v", conflicts, err)
	}
	if got := credits(one.TrackID); got != "artist:"+duet+" composer:ryo lyricist:ryo " {
		t.Fatalf("word undone: %s", got)
	}
	if proposals, _, _ = s.Splits(ctx); len(proposals) != 1 {
		t.Fatalf("proposed again after the undo: %+v", proposals)
	}
	// Kept whole, by word: no longer proposed.
	keep := []Part{}
	if _, err := s.DecideSplits(ctx, []SplitDecision{{Name: duet, Parts: &keep}}); err != nil {
		t.Fatal(err)
	}
	if got := credits(one.TrackID); got != "artist:"+duet+" composer:ryo lyricist:ryo " {
		t.Fatalf("kept whole: %s", got)
	}
	if proposals, _, _ = s.Splits(ctx); len(proposals) != 0 {
		t.Fatalf("proposed again: %+v", proposals)
	}
	if _, err := s.DecideSplits(ctx, []SplitDecision{{Name: "x", Parts: &[]Part{{Name: ""}}}}); err == nil {
		t.Fatal("an empty name was taken")
	}
}

// Songs from before composers were kept get them once, without touching a name already there.
func TestFillRoleNames(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	a, _ := s.Publish(ctx, verifiedAsset(t, s, "f1"), EntryInput{Title: "A", Artist: "X", Album: "One"})
	b, _ := s.Publish(ctx, verifiedAsset(t, s, "f2"), EntryInput{Title: "B", Artist: "X", Album: "One", Composer: "Kept"})
	if s.RoleNamesFilled(ctx) {
		t.Fatal("filled before it ran")
	}
	if err := s.FillRoleNames(ctx, map[int64]RoleNames{a.TrackID: {Composer: "梶浦由記", Arranger: "梶浦由記"},
		b.TrackID: {Composer: "Read", Lyricist: "Words"}}); err != nil {
		t.Fatal(err)
	}
	da, _ := s.Track(ctx, a.TrackID)
	db, _ := s.Track(ctx, b.TrackID)
	if da.Composer != "梶浦由記" || da.Arranger != "梶浦由記" || len(da.Credits) != 3 || db.Composer != "Kept" || db.Lyricist != "Words" {
		t.Fatalf("filled %+v / %+v", da, db)
	}
	if res, _ := s.Search(ctx, "梶浦", 10); len(res.Tracks) != 1 || len(res.Artists) != 1 {
		t.Fatalf("search after filling %+v", res)
	}
	if !s.RoleNamesFilled(ctx) {
		t.Fatal("not noted")
	}
}

// The word on a name goes with it when its artist is renamed, and back when the rename is undone;
// a new name with a word of its own keeps that one. One who voices two characters of a song has the
// song once, as both.
func TestSplitsFollowRenames(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	const band = "Earth, Wind & Fire"
	one, _ := s.Publish(ctx, verifiedAsset(t, s, "w1"), EntryInput{Title: "S", Artist: band, Album: "One"})
	keep := []Part{}
	if _, err := s.DecideSplits(ctx, []SplitDecision{{Name: band, Parts: &keep}}); err != nil {
		t.Fatal(err)
	}
	decidedAs := func(want ...string) {
		t.Helper()
		proposals, decided, err := s.Splits(ctx)
		var names []string
		for _, d := range decided {
			names = append(names, d.Name)
		}
		if err != nil || len(proposals) != 0 || fmt.Sprint(names) != fmt.Sprint(want) {
			t.Fatalf("proposals %+v, decided %v, want %v: %v", proposals, names, want, err)
		}
	}
	d, _ := s.Track(ctx, one.TrackID)
	rename, err := s.RenameArtist(ctx, d.Credits[0].ArtistID, band+" (band)")
	if err != nil {
		t.Fatal(err)
	}
	decidedAs(band + " (band)")
	if _, conflicts, err := s.Undo(ctx, rename); err != nil || len(conflicts) != 0 {
		t.Fatalf("undo: %+v %v", conflicts, err)
	}
	decidedAs(band)

	two, _ := s.Publish(ctx, verifiedAsset(t, s, "w2"), EntryInput{Title: "T", Artist: "A & B", Album: "One"})
	parts := []Part{{Name: "A"}, {Name: "B"}}
	if _, err := s.DecideSplits(ctx, []SplitDecision{{Name: "A & B", Parts: &parts}}); err != nil {
		t.Fatal(err)
	}
	d, _ = s.Track(ctx, one.TrackID)
	if _, err := s.RenameArtist(ctx, d.Credits[0].ArtistID, "A & B"); err != nil {
		t.Fatal(err)
	}
	decidedAs("A & B")
	for _, id := range []int64{one.TrackID, two.TrackID} {
		if d, _ := s.Track(ctx, id); len(d.Credits) != 2 || d.Credits[0].Name != "A" || d.Credits[1].Name != "B" {
			t.Fatalf("track %d: %+v", id, d.Credits)
		}
	}
	var n int
	s.db.QueryRow(`SELECT count(*) FROM artist_splits WHERE name = ?`, band).Scan(&n)
	if n != 1 {
		t.Fatal("the old name's word went")
	}

	const twice = "C (CV: X) & D (CV: X)"
	s.Publish(ctx, verifiedAsset(t, s, "w3"), EntryInput{Title: "U", Artist: twice, Album: "One"})
	both := Split(twice)
	if _, err := s.DecideSplits(ctx, []SplitDecision{{Name: twice, Parts: &both}}); err != nil {
		t.Fatal(err)
	}
	res, _ := s.Search(ctx, "X", 10)
	x, _ := s.ArtistDetail(ctx, res.Artists[0].ID)
	if len(x.Roles) != 1 || len(x.Roles[0].Items) != 1 || x.Roles[0].Items[0].As != "C、D" || len(x.Characters) != 2 {
		t.Fatalf("voice page %+v", x)
	}
}
