package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/mover"
)

// Every file of a folder store moved to another one, through the API: what it would do, asked for,
// followed until it is done, listed, and cleared; the folder can be removed then. What
// cannot be asked for is refused.
func TestMoves(t *testing.T) {
	s, h, token := storageServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.mover.Run(ctx)
	roots := []string{t.TempDir(), t.TempDir()}
	for i, root := range roots {
		cfg, _ := json.Marshal(map[string]string{"root": root})
		if rec := do(t, h, "POST", "/api/v1/storage", token, map[string]any{"kind": "local", "name": fmt.Sprint("Disk ", i+1), "mode": "managed",
			"config": json.RawMessage(cfg)}); rec.Code != http.StatusCreated {
			t.Fatalf("add: %d %s", rec.Code, rec.Body)
		}
	}
	data := []byte("the song's bytes")
	sum := sha256.Sum256(data)
	os.MkdirAll(filepath.Join(roots[0], "library", "x"), 0o755)
	os.WriteFile(filepath.Join(roots[0], "library", "x", "01 t.flac"), data, 0o644)
	a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data)), Format: "flac", Codec: "flac"})
	if err := s.lib.MarkVerified(ctx, a.ID, "local:1:library/x/01 t.flac"); err != nil {
		t.Fatal(err)
	}
	s.lib.Publish(ctx, a.ID, library.EntryInput{Title: "t", Artist: "x", Album: "A", AlbumArtist: "y"})

	for _, body := range []map[string]any{{"dest": "1", "origin": "1"}, {"dest": "7", "origin": "1"}, {"dest": "2"}} {
		if rec := do(t, h, "POST", "/api/v1/moves/preview", token, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: %d %s", body, rec.Code, rec.Body)
		}
	}
	rec := do(t, h, "POST", "/api/v1/moves/preview", token, map[string]any{"dest": "2", "origin": "1"})
	if p := decode[mover.Plan](t, rec.Body); rec.Code != 200 || p.Files != 1 || p.Songs != 1 || p.Bytes != int64(len(data)) || p.DestName != "Disk 2" {
		t.Fatalf("preview: %d %+v", rec.Code, p)
	}
	rec = do(t, h, "POST", "/api/v1/moves", token, map[string]any{"dest": "2", "origin": "1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	id := decode[mover.Move](t, rec.Body).ID
	var v mover.Move
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		v = decode[mover.Move](t, do(t, h, "GET", fmt.Sprintf("/api/v1/moves/%d", id), token, nil).Body)
		if v.State != mover.Running || time.Now().After(deadline) {
			break
		}
	}
	if v.State != mover.Done || v.Moved != 1 || v.Title != "Disk 1" {
		t.Fatalf("move %+v", v)
	}
	if b, err := os.ReadFile(filepath.Join(roots[1], "library", "y", "A", "01 t.flac")); err != nil || string(b) != string(data) {
		t.Fatalf("at the destination: %q %v", b, err)
	}
	if list := decode[struct{ Moves []mover.Move }](t, do(t, h, "GET", "/api/v1/moves", token, nil).Body); len(list.Moves) != 1 || list.Moves[0].ID != id {
		t.Fatalf("listed %+v", list.Moves)
	}
	if rec := do(t, h, "GET", "/api/v1/moves/99", token, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("no such move: %d", rec.Code)
	}
	if rec := do(t, h, "POST", fmt.Sprintf("/api/v1/moves/%d/clear", id), token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body)
	}
	if list := decode[struct{ Moves []mover.Move }](t, do(t, h, "GET", "/api/v1/moves", token, nil).Body); len(list.Moves) != 0 {
		t.Fatalf("after clear %+v", list.Moves)
	}
	if rec := do(t, h, "DELETE", "/api/v1/storage/1", token, nil); rec.Code >= 300 {
		t.Fatalf("remove the emptied folder: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", "/api/v1/storage", token, nil); strings.Contains(rec.Body.String(), "Disk 1") {
		t.Fatal("still there")
	}
}

// A location files are being moved to is not removed before the move is over or canceled (review #236).
func TestMoveKeepsDestination(t *testing.T) {
	s, h, token := storageServer(t)
	ctx := context.Background()
	roots := []string{t.TempDir(), t.TempDir()}
	for i, root := range roots {
		cfg, _ := json.Marshal(map[string]string{"root": root})
		if rec := do(t, h, "POST", "/api/v1/storage", token, map[string]any{"kind": "local", "name": fmt.Sprint("Disk ", i+1), "mode": "managed",
			"config": json.RawMessage(cfg)}); rec.Code != http.StatusCreated {
			t.Fatalf("add: %d %s", rec.Code, rec.Body)
		}
	}
	data := []byte("the song's bytes")
	sum := sha256.Sum256(data)
	os.MkdirAll(filepath.Join(roots[0], "library"), 0o755)
	os.WriteFile(filepath.Join(roots[0], "library", "t.flac"), data, 0o644)
	a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data)), Format: "flac", Codec: "flac"})
	s.lib.MarkVerified(ctx, a.ID, "local:1:library/t.flac")
	s.lib.Publish(ctx, a.ID, library.EntryInput{Title: "t", Artist: "x", Album: "A", AlbumArtist: "y"})
	rec := do(t, h, "POST", "/api/v1/moves", token, map[string]any{"dest": "2", "origin": "1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	id := decode[mover.Move](t, rec.Body).ID
	if rec := do(t, h, "DELETE", "/api/v1/storage/2", token, nil); rec.Code != http.StatusConflict {
		t.Fatalf("removed the destination of a move: %d %s", rec.Code, rec.Body)
	}
	type stores struct {
		Stores []struct {
			ID       int64 `json:"id"`
			MovingIn int   `json:"moving_in"`
		}
	}
	info := decode[stores](t, do(t, h, "GET", "/api/v1/storage", token, nil).Body)
	if len(info.Stores) != 2 || info.Stores[0].MovingIn != 0 || info.Stores[1].MovingIn != 1 {
		t.Fatalf("stores %+v", info.Stores)
	}
	if rec := do(t, h, "POST", fmt.Sprintf("/api/v1/moves/%d/cancel", id), token, nil); rec.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "DELETE", "/api/v1/storage/2", token, nil); rec.Code != http.StatusOK {
		t.Fatalf("remove once canceled: %d %s", rec.Code, rec.Body)
	}
}
