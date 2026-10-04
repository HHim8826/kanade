package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/HHim8826/kanade/server/internal/library"
)

// The category endpoints (review #92): made, filled in one edit, listed with their albums and the
// albums in none, and deleted as an edit that undo takes back.
func TestCategoryEndpoints(t *testing.T) {
	ctx := context.Background()
	s, h := newTestServer(t)
	tok := loginToken(t, s, h)
	album := func(sha, title string) int64 {
		a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: sha, Size: 1, Format: "flac", Codec: "flac"})
		s.lib.MarkVerified(ctx, a.ID, "d-"+sha)
		r, _ := s.lib.Publish(ctx, a.ID, library.EntryInput{Title: "t", Artist: "x", Album: title, AlbumArtist: "y"})
		var id int64
		s.db.QueryRow(`SELECT album_id FROM album_entries WHERE id = ?`, r.EntryID).Scan(&id)
		return id
	}
	A, B := album("a", "ARIA The BOX"), album("b", "Umineko")
	call := func(method, path string, body any, out any) int {
		t.Helper()
		rec := do(t, h, method, path, tok, body)
		if out != nil {
			json.Unmarshal(rec.Body.Bytes(), out)
		}
		return rec.Code
	}
	var cat library.Category
	if code := call("POST", "/api/v1/categories", map[string]string{"name": "ARIA"}, &cat); code != http.StatusCreated || cat.ID == 0 {
		t.Fatalf("create: %d %+v", code, cat)
	}
	if rec := do(t, h, "GET", "/api/v1/categories", tok, nil); strings.Contains(rec.Body.String(), "null") {
		t.Fatalf("lists are never null: %s", rec.Body)
	}
	if code := call("POST", "/api/v1/categories", map[string]string{"name": "aria"}, nil); code != http.StatusBadRequest {
		t.Fatalf("same name: %d", code)
	}
	var res struct {
		Group   int64             `json:"group"`
		Created *library.Category `json:"created"`
	}
	if code := call("POST", "/api/v1/albums/categorize", map[string]any{"albums": []int64{A}, "add": []int64{cat.ID}}, &res); code != 200 || res.Group == 0 {
		t.Fatalf("categorize: %d %+v", code, res)
	}
	if code := call("POST", "/api/v1/albums/categorize", map[string]any{"albums": []int64{B}, "create": "海貓"}, &res); code != 200 || res.Created == nil {
		t.Fatalf("categorize into a new one: %d %+v", code, res)
	}
	var albums []library.AlbumSummary
	if call("GET", "/api/v1/albums?category="+itoa(cat.ID), nil, &albums); len(albums) != 1 || albums[0].ID != A {
		t.Fatalf("in ARIA: %+v", albums)
	}
	if code := call("GET", "/api/v1/albums?category=x", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("bad category: %d", code)
	}
	var list library.CategoryList
	if call("GET", "/api/v1/categories?albums="+itoa(A)+","+itoa(B), nil, &list); len(list.Categories) != 2 || list.Uncategorized != 0 ||
		list.Categories[0].Selected != 1 {
		t.Fatalf("list: %+v", list)
	}
	if code := call("PATCH", "/api/v1/categories/"+itoa(cat.ID), map[string]string{"name": "ARIA シリーズ"}, nil); code != http.StatusNoContent {
		t.Fatalf("rename: %d", code)
	}
	if code := call("DELETE", "/api/v1/categories/"+itoa(cat.ID), nil, &res); code != 200 || res.Group == 0 {
		t.Fatalf("delete: %d", code)
	}
	if call("GET", "/api/v1/albums?category=none", nil, &albums); len(albums) != 1 || albums[0].ID != A {
		t.Fatalf("in none after delete: %+v", albums)
	}
	if code := call("POST", "/api/v1/edits/"+itoa(res.Group)+"/undo", nil, nil); code != 200 {
		t.Fatalf("undo delete: %d", code)
	}
	if call("GET", "/api/v1/albums?category="+itoa(cat.ID), nil, &albums); len(albums) != 1 {
		t.Fatalf("back after undo: %+v", albums)
	}
}
