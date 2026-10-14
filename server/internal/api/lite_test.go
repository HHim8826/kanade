package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/config"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/lite"
	"github.com/HHim8826/kanade/server/internal/settings"
	"github.com/HHim8826/kanade/server/internal/stream"
)

// memDrive serves Drive files from memory; a file listed in down fails.
type memDrive struct {
	mu    sync.Mutex
	files map[string][]byte
	down  map[string]bool
}

func (m *memDrive) OpenRange(ctx context.Context, id string, start, end int64) (*http.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.files[id]
	if !ok || m.down[id] {
		return nil, errors.New("drive is down")
	}
	if end < 0 || end >= int64(len(data)) {
		end = int64(len(data)) - 1
	}
	return &http.Response{StatusCode: http.StatusPartialContent, Body: io.NopCloser(bytes.NewReader(data[start : end+1]))}, nil
}

// pad ends each original: the "re-encoding" drops it, so what it makes is smaller.
const pad = "~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~"

// prefixer "re-encodes" by prefixing what it reads, less the padding; it fails for originals
// starting with "bad", and keeps the padding of those starting with "big" (no smaller). One starting
// with "slow" waits for its gate first (or the end of its making, counted in stopped).
type prefixer struct {
	gate    chan struct{}
	stopped *atomic.Int32
}

func (p prefixer) Opus(ctx context.Context, src string, in io.Reader, dst string, kbps int, downmix bool) error {
	data, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	if bytes.HasPrefix(data, []byte("slow")) {
		select {
		case <-p.gate:
		case <-ctx.Done():
			p.stopped.Add(1)
			return ctx.Err()
		}
	}
	if bytes.HasPrefix(data, []byte("bad")) {
		return errors.New("cannot decode")
	}
	if !bytes.HasPrefix(data, []byte("big")) {
		data = bytes.TrimRight(data, "~")
	}
	return os.WriteFile(dst, append([]byte("OggS lite of "), data...), 0o600)
}

// A song loaded ahead (pre=1: gapless playback), re-encoded or not, is not the song played until
// the player says it plays it (POST .../play).
func TestStreamLoadedAhead(t *testing.T) {
	drive := &memDrive{files: map[string][]byte{}, down: map[string]bool{}}
	s, h := newTestServer(t, func(d *Deps) {
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		cache, err := stream.NewCache(drive, d.Config.Path(config.DirCache), 64<<20, log)
		if err != nil {
			t.Fatal(err)
		}
		d.Cache = cache
		if d.Lite, err = lite.New(d.Config.Path(config.DirLite), LiteBudget, prefixer{}, cache, log); err != nil {
			t.Fatal(err)
		}
	})
	token := loginToken(t, s, h)
	a, b := addSong(t, s, drive, "a1", "flac", 900_000, "fLaC a"+pad), addSong(t, s, drive, "b1", "flac", 900_000, "fLaC b"+pad)
	get := func(path string) (int, string) {
		rec := do(t, h, "GET", path, token, nil)
		return rec.Code, rec.Body.String()
	}
	if code, body := get(fmt.Sprintf("/api/v1/stream/%d", a)); code != 200 || body != "fLaC a"+pad {
		t.Fatalf("a: %d %q", code, body)
	}
	if code, body := get(fmt.Sprintf("/api/v1/stream/%d?pre=1", b)); code != 200 || body != "fLaC b"+pad {
		t.Fatalf("b ahead: %d %q", code, body)
	}
	if cur, next := s.cache.Playing(); cur != "d-a1" || next != "d-b1" {
		t.Fatalf("b loaded ahead: playing %q, next %q", cur, next)
	}
	if code, body := get(fmt.Sprintf("/api/v1/stream/%d?q=%s&pre=1", b, lite.Quality)); code != 200 || body != "OggS lite of fLaC b" {
		t.Fatalf("b ahead in lite mode: %d %q", code, body)
	}
	if cur, _ := s.cache.Playing(); cur != "d-a1" {
		t.Fatalf("b loaded ahead in lite mode: playing %q", cur)
	}
	if rec := do(t, h, "POST", fmt.Sprintf("/api/v1/stream/%d/play", b), token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("play: %d", rec.Code)
	}
	if cur, _ := s.cache.Playing(); cur != "d-b1" {
		t.Fatalf("b played: playing %q", cur)
	}
	if rec := do(t, h, "POST", "/api/v1/stream/999999/play", token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("play of no song: %d", rec.Code)
	}
}

func TestStreamLite(t *testing.T) {
	drive := &memDrive{files: map[string][]byte{}, down: map[string]bool{}}
	s, h := newTestServer(t, func(d *Deps) {
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		cache, err := stream.NewCache(drive, d.Config.Path(config.DirCache), 64<<20, log)
		if err != nil {
			t.Fatal(err)
		}
		d.Cache = cache
		if d.Lite, err = lite.New(d.Config.Path(config.DirLite), LiteBudget, prefixer{}, cache, log); err != nil {
			t.Fatal(err)
		}
	})
	token := loginToken(t, s, h)
	song := func(sha, format string, bitrate int, data string) int64 {
		return addSong(t, s, drive, sha, format, bitrate, data+pad)
	}
	flac, mp3, bad, gone := song("f1", "flac", 900_000, "fLaC song"), song("m1", "mp3", 128_000, "ID3 small mp3"),
		song("b1", "flac", 900_000, "bad flac"), song("g1", "flac", 900_000, "fLaC gone")
	get := func(id int64, q, rng string) (*http.Response, string) {
		path := fmt.Sprintf("/api/v1/stream/%d", id)
		if q != "" {
			path += "?q=" + q
		}
		req, _ := http.NewRequest("GET", path, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		res := rec.Result()
		body, _ := io.ReadAll(res.Body)
		return res, string(body)
	}

	res, body := get(flac, lite.Quality, "")
	if res.StatusCode != 200 || body != "OggS lite of fLaC song" || res.Header.Get("Content-Type") != "audio/ogg" ||
		res.Header.Get("ETag") != `"f1-opus128"` || res.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("lite: %d %q %v", res.StatusCode, body, res.Header)
	}
	if res, body := get(flac, lite.Quality, "bytes=5-8"); res.StatusCode != 206 || body != "lite" ||
		res.Header.Get("Content-Range") != fmt.Sprintf("bytes 5-8/%d", len("OggS lite of fLaC song")) {
		t.Fatalf("lite range: %d %q %v", res.StatusCode, body, res.Header)
	}
	if res, body := get(flac, "", ""); body != "fLaC song"+pad || res.Header.Get("Content-Type") != "audio/flac" {
		t.Fatalf("original: %q %v", body, res.Header)
	}
	if res, body := get(mp3, lite.Quality, ""); body != "ID3 small mp3"+pad || res.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("a small lossy original is sent as it is: %q %v", body, res.Header)
	}
	if _, body := get(flac, "flac", ""); body != "fLaC song"+pad {
		t.Fatalf("an unknown quality: %q", body)
	}
	// One that cannot be re-encoded is sent as it is, every time.
	for range 2 {
		if res, body := get(bad, lite.Quality, ""); res.StatusCode != 200 || body != "bad flac"+pad {
			t.Fatalf("failed: %d %q", res.StatusCode, body)
		}
	}
	// Drive failing: a 503, as for the original.
	drive.down["d-g1"] = true
	if res, body := get(gone, lite.Quality, ""); res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "drive is down") {
		t.Fatalf("drive down: %d %q", res.StatusCode, body)
	}

	// Preloaded in lite mode: made ahead, and the original's preload is let go of.
	next := song("n1", "flac", 900_000, "fLaC next")
	if rec := do(t, h, "POST", fmt.Sprintf("/api/v1/stream/%d/prefetch", next), token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("prefetch: %d", rec.Code)
	}
	if rec := do(t, h, "POST", fmt.Sprintf("/api/v1/stream/%d/prefetch?q=%s", next, lite.Quality), token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("lite prefetch: %d", rec.Code)
	}
	if _, n := s.cache.Playing(); n != "" {
		t.Fatalf("the original is still preloaded: %q", n)
	}
	for end := time.Now().Add(5 * time.Second); s.lite.Used() < int64(len("OggS lite of fLaC next")+len("OggS lite of fLaC song")); {
		if time.Now().After(end) {
			t.Fatalf("not made ahead: %d bytes", s.lite.Used())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if rec := do(t, h, "GET", "/api/v1/status", token, nil); !strings.Contains(rec.Body.String(), `"lite":true`) {
		t.Fatalf("status: %s", rec.Body)
	}
	s.settings = &settings.Store{DB: s.db}
	rec := do(t, h, "GET", "/api/v1/settings", token, nil)
	if !strings.Contains(rec.Body.String(), `"lite_bytes":`) {
		t.Fatalf("settings: %s", rec.Body)
	}
	rec = do(t, h, "POST", fmt.Sprintf("/api/v1/stream/%d/url?q=%s", flac, lite.Quality), token, nil)
	if !strings.Contains(rec.Body.String(), `\u0026q=opus128"`) {
		t.Fatalf("signed url: %s", rec.Body)
	}
}

func addSong(t *testing.T, s *Server, drive *memDrive, sha, format string, bitrate int, data string) int64 {
	t.Helper()
	ctx := context.Background()
	a, err := s.lib.CreateAsset(ctx, library.Asset{SHA256: sha, Size: int64(len(data)), Format: format, Codec: format, Bitrate: bitrate, Channels: 2})
	if err != nil {
		t.Fatal(err)
	}
	drive.mu.Lock()
	drive.files["d-"+sha] = []byte(data)
	drive.mu.Unlock()
	s.lib.MarkVerified(ctx, a.ID, "d-"+sha)
	return a.ID
}

// What a player in lite mode is told it got (review #209) is what it got: re-encoded, or as it is
// (lossy and small, lossless at a low bitrate, not smaller re-encoded (review #212), failed). And
// whichever way a song comes, it is the one played: what was being made for the one before stops
// unless waited for or preloaded (review #208).
func TestStreamLiteFollowsPlay(t *testing.T) {
	drive := &memDrive{files: map[string][]byte{}, down: map[string]bool{}}
	enc := prefixer{gate: make(chan struct{}), stopped: &atomic.Int32{}}
	s, h := newTestServer(t, func(d *Deps) {
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		cache, err := stream.NewCache(drive, d.Config.Path(config.DirCache), 64<<20, log)
		if err != nil {
			t.Fatal(err)
		}
		d.Cache = cache
		if d.Lite, err = lite.New(d.Config.Path(config.DirLite), LiteBudget, enc, cache, log); err != nil {
			t.Fatal(err)
		}
	})
	token := loginToken(t, s, h)
	song := func(sha, format string, bitrate int, data string) int64 {
		return addSong(t, s, drive, sha, format, bitrate, data+pad)
	}
	stream := func(ctx context.Context, id int64, q string) (int, string, string) {
		path := fmt.Sprintf("/api/v1/stream/%d", id)
		if q != "" {
			path += "?q=" + q
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Header().Get("Content-Type"), rec.Body.String()
	}
	told := func(id int64) string {
		rec := do(t, h, "GET", fmt.Sprintf("/api/v1/stream/%d/lite", id), token, nil)
		if rec.Code != 200 {
			t.Fatalf("lite of %d: %d %s", id, rec.Code, rec.Body)
		}
		return strings.TrimSpace(rec.Body.String())
	}
	ctx := context.Background()

	flac, mp3, low, big, bad := song("f1", "flac", 900_000, "fLaC song"), song("m1", "mp3", 128_000, "ID3 small mp3"),
		song("l1", "flac", 108_521, "fLaC quiet"), song("g1", "flac", 0, "big flac"), song("b1", "flac", 900_000, "bad flac")
	if got := told(flac); got != `{"later":false,"lite":false}` {
		t.Fatalf("not played yet: %s", got)
	}
	for _, c := range []struct {
		id        int64
		typ, lite string
		reEncoded bool
		what      string
	}{{flac, "audio/ogg", `{"later":false,"lite":true}`, true, "re-encoded"}, {mp3, "audio/mpeg", `{"later":false,"lite":false}`, false, "lossy and small"},
		{low, "audio/flac", `{"later":false,"lite":false}`, false, "lossless at a low bitrate"}, {big, "audio/flac", `{"later":false,"lite":false}`, false, "not smaller"},
		{bad, "audio/flac", `{"later":false,"lite":false}`, false, "failed"}} {
		for range 2 { // and again a while later: the same
			if code, typ, _ := stream(ctx, c.id, lite.Quality); code != 200 || typ != c.typ {
				t.Fatalf("%s: %d %s", c.what, code, typ)
			}
			if got := told(c.id); got != c.lite {
				t.Fatalf("%s: told %s", c.what, got)
			}
		}
	}
	if code, _, body := stream(ctx, big, ""); code != 200 || body != "big flac"+pad {
		t.Fatalf("big, lite mode off: %d %q", code, body)
	}

	// A, re-encoded and not made yet, its request gone; then B played as it is: A stops.
	type step struct {
		what string
		play func()
	}
	n := 0
	for _, st := range []step{
		{"an original (lite mode off)", func() { stream(ctx, flac, "") }},
		{"a small lossy one, lite mode on", func() { stream(ctx, mp3, lite.Quality) }},
		{"another re-encoded", func() { stream(ctx, flac, lite.Quality) }},
	} {
		n++
		a := song(fmt.Sprint("s", n), "flac", 900_000, "slow song")
		rctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { stream(rctx, a, lite.Quality); close(done) }()
		time.Sleep(50 * time.Millisecond) // A being made
		cancel()
		<-done
		time.Sleep(20 * time.Millisecond)
		if enc.stopped.Load() != int32(n-1) {
			t.Fatalf("%s: A stopped before B played", st.what)
		}
		st.play()
		for end := time.Now().Add(5 * time.Second); enc.stopped.Load() != int32(n); {
			if time.Now().After(end) {
				t.Fatalf("%s: A goes on", st.what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// A preloaded goes on while another plays as it is.
	next := song("p1", "flac", 900_000, "slow next")
	if rec := do(t, h, "POST", fmt.Sprintf("/api/v1/stream/%d/prefetch?q=%s", next, lite.Quality), token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("prefetch: %d", rec.Code)
	}
	time.Sleep(50 * time.Millisecond)
	stream(ctx, mp3, "")
	time.Sleep(50 * time.Millisecond)
	if enc.stopped.Load() != int32(n) {
		t.Fatal("the song preloaded stopped")
	}
	close(enc.gate)
	if code, typ, _ := stream(ctx, next, lite.Quality); code != 200 || typ != "audio/ogg" || told(next) != `{"later":false,"lite":true}` {
		t.Fatalf("the song preloaded: %d %s", code, typ)
	}
}

// Without a lite store (no FFmpeg with Opus), a player in lite mode gets the originals.
func TestStreamLiteUnavailable(t *testing.T) {
	ctx := context.Background()
	drive := &memDrive{files: map[string][]byte{"d-f1": []byte("fLaC song")}}
	s, h := newTestServer(t, func(d *Deps) {
		cache, _ := stream.NewCache(drive, d.Config.Path(config.DirCache), 64<<20, slog.New(slog.NewTextHandler(io.Discard, nil)))
		d.Cache = cache
	})
	token := loginToken(t, s, h)
	a, _ := s.lib.CreateAsset(ctx, library.Asset{SHA256: "f1", Size: 9, Format: "flac", Codec: "flac"})
	s.lib.MarkVerified(ctx, a.ID, "d-f1")
	if rec := do(t, h, "GET", fmt.Sprintf("/api/v1/stream/%d?q=%s", a.ID, lite.Quality), token, nil); rec.Body.String() != "fLaC song" {
		t.Fatalf("got %d %q", rec.Code, rec.Body)
	}
	if rec := do(t, h, "GET", fmt.Sprintf("/api/v1/stream/%d/lite", a.ID), token, nil); strings.TrimSpace(rec.Body.String()) != `{"later":false,"lite":false}` {
		t.Fatalf("lite: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", fmt.Sprintf("/api/v1/stream/%d/prefetch?q=%s", a.ID, lite.Quality), token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("prefetch: %d", rec.Code)
	}
	if _, n := s.cache.Playing(); n != "d-f1" {
		t.Fatalf("the original is not preloaded: %q", n)
	}
	if rec := do(t, h, "DELETE", "/api/v1/stream/prefetch", token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("unprefetch: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/v1/status", token, nil); !strings.Contains(rec.Body.String(), `"lite":false`) {
		t.Fatalf("status: %s", rec.Body)
	}
	if rec := do(t, h, "POST", "/api/v1/cache/trim", token, nil); rec.Code != 200 {
		t.Fatalf("trim: %d", rec.Code)
	}
}

// A long song not made yet starts at once, sent as it is (the player says so), and is made
// meanwhile: re-encoded the next time it is played, not while the same playing seeks (review #226).
func TestStreamLiteLongSong(t *testing.T) {
	drive := &memDrive{files: map[string][]byte{}, down: map[string]bool{}}
	var dir string
	s, h := newTestServer(t, func(d *Deps) {
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		cache, err := stream.NewCache(drive, d.Config.Path(config.DirCache), 64<<20, log)
		if err != nil {
			t.Fatal(err)
		}
		d.Cache = cache
		dir = d.Config.Path(config.DirLite)
		if d.Lite, err = lite.New(dir, LiteBudget, prefixer{}, cache, log); err != nil {
			t.Fatal(err)
		}
	})
	token := loginToken(t, s, h)
	long, short := addSong(t, s, drive, "l1", "flac", 900_000, "fLaC drama"+pad), addSong(t, s, drive, "s1", "flac", 900_000, "fLaC song"+pad)
	s.db.Exec(`UPDATE assets SET duration_ms = 1800000 WHERE sha256 = 'l1'`)
	get := func(path, rng string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body, _ := io.ReadAll(rec.Result().Body)
		return rec.Result(), string(body)
	}
	stream := func(id int64, rng string) (*http.Response, string) {
		return get(fmt.Sprintf("/api/v1/stream/%d?q=%s", id, lite.Quality), rng)
	}
	said := func(id int64) string {
		_, body := get(fmt.Sprintf("/api/v1/stream/%d/lite", id), "")
		return strings.TrimSpace(body)
	}
	if res, body := stream(long, "bytes=0-"); res.Header.Get("Content-Type") != "audio/flac" || body != "fLaC drama"+pad {
		t.Fatalf("a long song: %q %v", body, res.Header)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "l1.opus.ogg")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not made meanwhile")
		}
	}
	if res, body := stream(long, "bytes=5-"); res.StatusCode != 206 || body != "drama"+pad || said(long) != `{"later":true,"lite":false}` {
		t.Fatalf("the same playing sought, once made: %d %q", res.StatusCode, body)
	}
	if res, body := stream(short, ""); body != "OggS lite of fLaC song" {
		t.Fatalf("a short song: %q %v", body, res.Header)
	}
	if res, body := stream(long, "bytes=0-"); body != "OggS lite of fLaC drama" || said(long) != `{"later":false,"lite":true}` {
		t.Fatalf("the next time: %q %v", body, res.Header)
	}
	// Played from the middle, not made: as it is, every request of the playing alike.
	mid := addSong(t, s, drive, "m1", "flac", 900_000, "fLaC from the middle"+pad)
	if res, body := stream(mid, "bytes=5-"); res.StatusCode != 206 || body != "from the middle"+pad || said(mid) != `{"later":true,"lite":false}` {
		t.Fatalf("from the middle: %d %q", res.StatusCode, body)
	}
}

// A playback named by its stream (?play=) keeps what its first request got: the original of a long
// song, its ranges of the same file, after another player played another song once the song was
// made; another playback gets it re-encoded (review #242).
func TestStreamLitePlaybackKept(t *testing.T) {
	drive := &memDrive{files: map[string][]byte{}, down: map[string]bool{}}
	var dir string
	s, h := newTestServer(t, func(d *Deps) {
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		cache, err := stream.NewCache(drive, d.Config.Path(config.DirCache), 64<<20, log)
		if err != nil {
			t.Fatal(err)
		}
		d.Cache = cache
		dir = d.Config.Path(config.DirLite)
		if d.Lite, err = lite.New(dir, LiteBudget, prefixer{}, cache, log); err != nil {
			t.Fatal(err)
		}
	})
	token := loginToken(t, s, h)
	long, other := addSong(t, s, drive, "l1", "flac", 900_000, "fLaC drama"+pad), addSong(t, s, drive, "o1", "flac", 900_000, "fLaC other"+pad)
	s.db.Exec(`UPDATE assets SET duration_ms = 1800000 WHERE sha256 = 'l1'`)
	get := func(path, rng string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body, _ := io.ReadAll(rec.Result().Body)
		return rec.Result(), string(body)
	}
	stream := func(id int64, play, rng string) (*http.Response, string) {
		return get(fmt.Sprintf("/api/v1/stream/%d?q=%s&play=%s", id, lite.Quality, play), rng)
	}
	if res, body := stream(long, "A", "bytes=0-"); res.Header.Get("Content-Type") != "audio/flac" || body != "fLaC drama"+pad {
		t.Fatalf("a long song: %q %v", body, res.Header)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "l1.opus.ogg")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not made meanwhile")
		}
	}
	if _, body := stream(other, "B", ""); body != "OggS lite of fLaC other" { // another player
		t.Fatalf("another player: %q", body)
	}
	res, body := stream(long, "A", "bytes=5-")
	if res.StatusCode != 206 || res.Header.Get("Content-Type") != "audio/flac" || body != "drama"+pad {
		t.Fatalf("the first playback sought: %d %q %v", res.StatusCode, body, res.Header)
	}
	if _, said := get(fmt.Sprintf("/api/v1/stream/%d/lite?play=A", long), ""); strings.TrimSpace(said) != `{"later":true,"lite":false}` {
		t.Fatalf("the first playback said %s", said)
	}
	if _, body := stream(long, "C", "bytes=0-"); body != "OggS lite of fLaC drama" {
		t.Fatalf("another playback: %q", body)
	}
	if _, said := get(fmt.Sprintf("/api/v1/stream/%d/lite?play=C", long), ""); strings.TrimSpace(said) != `{"later":false,"lite":true}` {
		t.Fatalf("another playback said %s", said)
	}
}
