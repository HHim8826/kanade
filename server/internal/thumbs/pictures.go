package thumbs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// A gallery's pictures (#134) are often scans a browser cannot show (TIFF, BMP), or too large to show
// as they are: they are shown by a viewing copy (ViewSize), and their sizes are made from the
// original by FFmpeg (review #248), each from a copy of the original made aside. Two originals are
// read at once at most, and one picture is made at a time; who waits the longest is not who goes
// first: one being looked at now (Looking) goes before a size for a page (the default), and those
// before one made ahead (Ahead, an import).

// NeedsView says whether a gallery picture is shown by its viewing copy, and its sizes made with
// Picture: one a browser cannot show (TIFF, BMP), or too large to show as it is.
func NeedsView(mime string, width, height int, size int64) bool {
	web := mime == "image/jpeg" || mime == "image/png" || mime == "image/webp"
	return !web || max(width, height) > ViewSize || size > 16<<20
}

// How soon what is asked for is made, by the context it is asked with.
const (
	ahead  int32 = iota // made ahead of being asked for
	shown               // for a page to show
	looked              // someone looks at it now
)

type levelKey struct{}

// Looking marks what is asked for with ctx as being looked at now: it is made before the others.
func Looking(ctx context.Context) context.Context { return context.WithValue(ctx, levelKey{}, looked) }

// Ahead marks what is asked for with ctx as made ahead of being asked for: the others go first.
func Ahead(ctx context.Context) context.Context { return context.WithValue(ctx, levelKey{}, ahead) }

func level(ctx context.Context) int32 {
	if v, ok := ctx.Value(levelKey{}).(int32); ok {
		return v
	}
	return shown
}

// Picture is a gallery picture that NeedsView at size (a Size), from the folder or made: by FFmpeg
// from the original that open reads, or, when its viewing copy is here already (or there is no
// FFmpeg), resized from that copy.
func (s *Store) Picture(ctx context.Context, sha string, size int, open func(context.Context) (io.ReadCloser, error)) ([]byte, error) {
	if _, err := os.Stat(filepath.Join(s.dir, sha+"-view.jpg")); err == nil || s.pictures == nil || size == 0 || size == ViewSize {
		if size == ViewSize {
			return s.Get(ctx, sha, size, open)
		}
		return s.Get(ctx, sha, size, func(ctx context.Context) (io.ReadCloser, error) {
			view, err := s.Get(ctx, sha, ViewSize, open)
			if err != nil {
				return nil, err
			}
			return io.NopCloser(bytes.NewReader(view)), nil
		})
	}
	name := fmt.Sprintf("%s-%d.jpg", sha, size)
	return s.do(ctx, sha, true, name, func(ctx context.Context, c *call) ([]byte, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		return s.made(sha, filepath.Join(s.dir, name), func() ([]byte, error) { return s.picture(ctx, c, open, size) })
	})
}

// picture makes a JPEG of the picture open reads, at most longest pixels on its longest side: the
// original copied aside, made a JPEG of by FFmpeg (a large scan takes some hundred MB to decode);
// without FFmpeg, decoded and resized here.
func (s *Store) picture(ctx context.Context, c *call, open func(context.Context) (io.ReadCloser, error), longest int) ([]byte, error) {
	if s.pictures == nil {
		orig, err := s.original(ctx, open)
		if err != nil {
			return nil, err
		}
		return s.resize(ctx, orig, longest)
	}
	got, err := s.reads.take(ctx, c)
	if err != nil {
		return nil, err
	}
	defer got()
	dir, err := os.MkdirTemp(s.dir, ".view-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	src, dst := filepath.Join(dir, "original"), filepath.Join(dir, "out.jpg")
	if err := s.copyOriginal(ctx, open, src); err != nil {
		return nil, err
	}
	done, err := s.makes.take(ctx, c)
	if err != nil {
		return nil, err
	}
	defer done()
	if err := s.pictures(ctx, src, dst, longest); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrUndecodable, err)
	}
	return os.ReadFile(dst)
}

// made keeps what build makes at path; a picture that cannot be read is not tried again for a while.
func (s *Store) made(sha, path string, build func() ([]byte, error)) ([]byte, error) {
	data, err := build()
	if errors.Is(err, ErrUndecodable) {
		s.mu.Lock()
		s.bad[sha] = time.Now().Add(badFor)
		s.mu.Unlock()
	}
	if err != nil {
		return nil, err
	}
	if err := s.keep(path, data); err != nil {
		s.log.Warn("keep a picture", "file", filepath.Base(path), "err", err) // served all the same
	}
	return data, nil
}

// gate lets n through at a time; of those waiting, the call asked for the soonest goes first, then
// the one waiting the longest.
type gate struct {
	mu      sync.Mutex
	free    int
	waiting []*waiter
}

type waiter struct {
	turn chan struct{}
	c    *call
}

func newGate(n int) *gate { return &gate{free: n} }

// take waits for a turn for c; none for one given up.
func (g *gate) take(ctx context.Context, c *call) (func(), error) {
	g.mu.Lock()
	if g.free > 0 && len(g.waiting) == 0 {
		g.free--
		g.mu.Unlock()
		return g.once(), nil
	}
	w := &waiter{turn: make(chan struct{}), c: c}
	g.waiting = append(g.waiting, w)
	g.mu.Unlock()
	select {
	case <-w.turn:
		if err := ctx.Err(); err != nil {
			g.give()
			return nil, err
		}
		return g.once(), nil
	case <-ctx.Done():
		g.mu.Lock()
		if i := slices.Index(g.waiting, w); i >= 0 {
			g.waiting = slices.Delete(g.waiting, i, i+1)
			g.mu.Unlock()
			return nil, ctx.Err()
		}
		g.mu.Unlock()
		g.give() // its turn came meanwhile: it goes to the next
		return nil, ctx.Err()
	}
}

func (g *gate) once() func() {
	var o sync.Once
	return func() { o.Do(g.give) }
}

// give hands a turn on: to the waiter asked for the soonest, else back.
func (g *gate) give() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.waiting) == 0 {
		g.free++
		return
	}
	best := 0
	for i, w := range g.waiting {
		if w.c.level.Load() > g.waiting[best].c.level.Load() {
			best = i
		}
	}
	w := g.waiting[best]
	g.waiting = slices.Delete(g.waiting, best, best+1)
	close(w.turn)
}
