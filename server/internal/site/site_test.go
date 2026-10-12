package site

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HHim8826/kanade/server/internal/db"
)

func open(t *testing.T) (*Store, func() *Store) {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	again := func() *Store {
		s, err := Open(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	return again(), again
}

// picture is w × h: red on the left half, blue on the right, with a green middle column.
func picture(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		c := color.RGBA{255, 0, 0, 255}
		if x >= w/2 {
			c = color.RGBA{0, 0, 255, 255}
		}
		if x >= w/2-2 && x < w/2+2 {
			c = color.RGBA{0, 255, 0, 255}
		}
		for y := range h {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decode(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestName(t *testing.T) {
	ctx := context.Background()
	s, again := open(t)
	if s.Name() != Default || (*Store)(nil).Name() != Default {
		t.Fatalf("default: %q", s.Name())
	}
	if err := s.SetName(ctx, "  奏の音楽室 "); err != nil || s.Name() != "奏の音楽室" {
		t.Fatalf("set: %v %q", err, s.Name())
	}
	if again().Name() != "奏の音楽室" {
		t.Fatal("not kept")
	}
	for _, bad := range []string{strings.Repeat("音", MaxName+1), "a\nb", "a\x00b", "\xff"} {
		if err := s.SetName(ctx, bad); !errors.Is(err, ErrName) || s.Name() != "奏の音楽室" {
			t.Fatalf("%q: %v, now %q", bad, err, s.Name())
		}
	}
	if err := s.SetName(ctx, strings.Repeat("音", MaxName)); err != nil {
		t.Fatalf("%d characters: %v", MaxName, err)
	}
	for _, back := range []string{"", "   ", Default} {
		s.SetName(ctx, "x")
		if err := s.SetName(ctx, back); err != nil || s.Name() != Default || again().Name() != Default {
			t.Fatalf("%q: %v %q", back, err, s.Name())
		}
	}
}

func TestIcon(t *testing.T) {
	ctx := context.Background()
	s, again := open(t)
	if s.Icon() != "" {
		t.Fatalf("built-in: %q", s.Icon())
	}
	if _, ok := s.IconFile("icon-192.png"); ok {
		t.Fatal("a file without an icon")
	}
	// A wide picture: its middle square, the green column in the middle.
	if err := s.SetIcon(ctx, bytes.NewReader(picture(t, 300, 200))); err != nil {
		t.Fatal(err)
	}
	v := s.Icon()
	if len(v) != 12 || again().Icon() != v {
		t.Fatalf("version %q, after reopening %q", v, again().Icon())
	}
	sizes := map[string]int{"icon-32.png": 32, "icon-180.png": 180, "icon-192.png": 192, "icon-512.png": 512, "icon-maskable.png": 512}
	for _, name := range Files {
		b, ok := s.IconFile(name)
		if !ok {
			t.Fatalf("%s missing", name)
		}
		img := decode(t, b)
		if n := sizes[name]; img.Bounds().Dx() != n || img.Bounds().Dy() != n {
			t.Fatalf("%s: %v", name, img.Bounds())
		}
	}
	icon := decode(t, must(s.IconFile("icon-512.png")))
	if r, g, b, _ := icon.At(256, 256).RGBA(); g < 0xf000 || r > 0x1000 || b > 0x1000 {
		t.Fatalf("not the middle square: %x %x %x", r, g, b)
	}
	if r, _, _, _ := icon.At(5, 256).RGBA(); r < 0xf000 {
		t.Fatal("left is not red")
	}
	// The maskable one: the picture in the middle 80%, on the average of its edges.
	m := decode(t, must(s.IconFile("icon-maskable.png")))
	if r, g, b, a := m.At(10, 10).RGBA(); a != 0xffff || r < 0x7000 || r > 0x8f00 || b < 0x7000 || b > 0x8f00 || g > 0x1000 {
		t.Fatalf("maskable backdrop: %x %x %x %x", r, g, b, a)
	}
	if r, _, _, _ := m.At(60, 256).RGBA(); r < 0xf000 {
		t.Fatal("maskable: the picture is not inside")
	}

	// Transparent edges: on white, where it cannot be transparent.
	clear := image.NewNRGBA(image.Rect(0, 0, 100, 100))
	for x := 30; x < 70; x++ {
		for y := 30; y < 70; y++ {
			clear.Set(x, y, color.NRGBA{0, 0, 0, 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, clear)
	if err := s.SetIcon(ctx, &buf); err != nil || s.Icon() == v {
		t.Fatalf("again: %v %q", err, s.Icon())
	}
	if _, _, _, a := decode(t, must(s.IconFile("icon-32.png"))).At(0, 0).RGBA(); a != 0 {
		t.Fatal("the tab's icon lost its transparency")
	}
	if c := color.RGBAModel.Convert(decode(t, must(s.IconFile("icon-180.png"))).At(0, 0)).(color.RGBA); c != (color.RGBA{255, 255, 255, 255}) {
		t.Fatalf("iPhone's: %v", c)
	}

	webp, _ := base64.StdEncoding.DecodeString("UklGRiQAAABXRUJQVlA4TBcAAAAvX8APAAdQs86Uuf8BICH8ny9F9D/1BwA=") // 96 × 64
	if err := s.SetIcon(ctx, bytes.NewReader(webp)); err != nil {
		t.Fatalf("webp: %v", err)
	}
	if r, g, b, _ := decode(t, must(s.IconFile("icon-512.png"))).At(100, 100).RGBA(); r>>8 != 0x33 || g>>8 != 0x66 || b>>8 != 0xcc {
		t.Fatalf("webp: %x %x %x", r>>8, g>>8, b>>8)
	}

	now := s.Icon()
	var g bytes.Buffer
	gif.Encode(&g, image.NewPaletted(image.Rect(0, 0, 100, 100), []color.Color{color.Black}), nil)
	huge := image.NewGray(image.Rect(0, 0, 5001, 5001))
	var h bytes.Buffer
	png.Encode(&h, huge)
	for _, c := range []struct {
		name string
		data []byte
		want error
	}{
		{"text", []byte("not an image"), ErrFormat},
		{"gif", g.Bytes(), ErrFormat},
		{"small", picture(t, 200, 63), ErrSmall},
		{"huge", h.Bytes(), ErrLarge},
		{"long", append(picture(t, 100, 100), make([]byte, MaxUpload)...), ErrLarge},
	} {
		if err := s.SetIcon(ctx, bytes.NewReader(c.data)); !errors.Is(err, c.want) || s.Icon() != now {
			t.Fatalf("%s: %v, icon %q", c.name, err, s.Icon())
		}
	}

	if err := s.ClearIcon(ctx); err != nil || s.Icon() != "" || again().Icon() != "" {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := s.IconFile("icon-32.png"); ok {
		t.Fatal("a file after clearing")
	}
}

func must(b []byte, ok bool) []byte {
	if !ok {
		panic("missing")
	}
	return b
}
