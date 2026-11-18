package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/HHim8826/kanade/server/internal/library"
)

// The pairs of names that look like one artist, and the names that look like several, come as
// many as asked for with how many there are; one artist's pairs alone for the merge dialog (#282).
func TestSameAndSplitsLimited(t *testing.T) {
	ctx := context.Background()
	s, h := newTestServer(t)
	tok := loginToken(t, s, h)
	ids := map[string]int64{}
	for i, name := range []string{"Aoi Hoshino", "Hoshino Aoi", "あおい", "Sora Kudo", "Kudo Sora", "Mika & Rin", "Yui & Nao", "Ken & Ann"} {
		sha := fmt.Sprint("same", i)
		a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: sha, Size: 1, Format: "flac", Codec: "flac"})
		s.lib.MarkVerified(ctx, a.ID, "d-"+sha)
		r, err := s.lib.Publish(ctx, a.ID, library.EntryInput{Title: name, Artist: name, Album: name, TrackNo: 1})
		if err != nil {
			t.Fatal(err)
		}
		d, _ := s.lib.Track(ctx, r.TrackID)
		ids[name] = d.Credits[0].ArtistID
	}
	type pairs struct {
		Pairs []library.SameArtist `json:"pairs"`
		Total int                  `json:"total"`
	}
	all := decode[pairs](t, do(t, h, "GET", "/api/v1/artists/same", tok, nil).Body)
	if all.Total < 2 || len(all.Pairs) != all.Total {
		t.Fatalf("all pairs: %+v", all)
	}
	first := decode[pairs](t, do(t, h, "GET", "/api/v1/artists/same?limit=1", tok, nil).Body)
	if first.Total != all.Total || len(first.Pairs) != 1 || first.Pairs[0] != all.Pairs[0] {
		t.Fatalf("the first: %+v", first)
	}
	mine := decode[pairs](t, do(t, h, "GET", fmt.Sprintf("/api/v1/artists/same?artist=%d", ids["Sora Kudo"]), tok, nil).Body)
	if mine.Total != 1 || len(mine.Pairs) != 1 || mine.Pairs[0].A.ID+mine.Pairs[0].B.ID != ids["Sora Kudo"]+ids["Kudo Sora"] {
		t.Fatalf("one artist's: %+v", mine)
	}
	type splits struct {
		Proposals []library.SplitName  `json:"proposals"`
		Decided   *[]library.SplitName `json:"decided"`
		Total     int                  `json:"total"`
	}
	rec := do(t, h, "GET", "/api/v1/artists/splits?limit=2", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("splits: %d %s", rec.Code, rec.Body)
	}
	if sp := decode[splits](t, rec.Body); sp.Total != 3 || len(sp.Proposals) != 2 || sp.Decided != nil {
		t.Fatalf("the first names to divide: %+v", sp)
	}
}
