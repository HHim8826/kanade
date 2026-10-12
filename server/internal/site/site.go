// Package site is how the service names and pictures itself, as its owner set it: the name the
// pages, the installed app, Discord and passkeys show (Kanade unless another was given), and the
// icon of tabs, home screens and the login page (the built-in one unless an image was uploaded).
// Both are kept in the settings table, so the daily database backup carries them.
package site

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg" // icons may be uploaded as JPEG, PNG or WebP
	"image/png"
	"io"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/HHim8826/kanade/server/internal/db"
)

const (
	Default = "Kanade"
	MaxName = 32 // characters

	MaxUpload = 10 << 20
	maxPixels = 25_000_000 // as for covers: a larger image is not decoded on a small VPS
	minSide   = 64
	side      = 512 // the icon kept: a square this size, as PNG

	keyName = "site.name"
	keyIcon = "site.icon" // the icon kept, base64
)

var (
	ErrName   = errors.New("a site name is 1 to 32 characters, without control characters")
	ErrFormat = errors.New("the icon must be a PNG, JPEG or WebP image")
	ErrSmall  = fmt.Errorf("the icon must be at least %d pixels on its shorter side", minSide)
	ErrLarge  = errors.New("the icon image is too large (10 MB, 25 megapixels at most)")
)

// Files are the sizes of an uploaded icon served: the tab's, the iPhone home screen's (no
// transparency: iOS fills it black), the installed app's, and the installed app's for launchers
// that cut icons to a shape (the picture kept inside the part none of them cut).
var Files = []string{"icon-32.png", "icon-180.png", "icon-192.png", "icon-512.png", "icon-maskable.png"}

type Store struct {
	db *sql.DB

	mu    sync.RWMutex
	name  string // "" for Default
	icon  []byte // the uploaded icon, side × side PNG; nil for the built-in one
	ver   string // a hash of icon: its files' addresses change with it
	files map[string][]byte
}

// Open reads the name and icon saved.
func Open(ctx context.Context, d *sql.DB) (*Store, error) {
	s := &Store{db: d}
	name, err := db.GetSetting(ctx, d, keyName)
	if err != nil {
		return nil, err
	}
	raw, err := db.GetSetting(ctx, d, keyIcon)
	if err != nil {
		return nil, err
	}
	var icon []byte
	if raw != "" {
		if icon, err = base64.StdEncoding.DecodeString(raw); err != nil {
			return nil, fmt.Errorf("site icon: %w", err)
		}
	}
	s.set(name, icon)
	return s, nil
}

func (s *Store) set(name string, icon []byte) {
	s.name, s.icon, s.ver, s.files = name, icon, "", map[string][]byte{}
	if icon != nil {
		sum := sha256.Sum256(icon)
		s.ver = hex.EncodeToString(sum[:6])
	}
}

// Name is the site's name. A nil store (tests, tools) is named Default.
func (s *Store) Name() string {
	if s == nil {
		return Default
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.name == "" {
		return Default
	}
	return s.name
}

// Icon is the uploaded icon's version, "" for the built-in icon.
func (s *Store) Icon() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ver
}

// IconFile is one of Files of the uploaded icon, made the first time it is asked for.
func (s *Store) IconFile(name string) ([]byte, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.icon == nil {
		return nil, false
	}
	if b, ok := s.files[name]; ok {
		return b, true
	}
	b, err := variant(s.icon, name)
	if err != nil {
		return nil, false
	}
	s.files[name] = b
	return b, true
}

// CleanName is name as kept: spaces trimmed, and "" (or Default) for the default.
func CleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == Default {
		return "", nil
	}
	if utf8.RuneCountInString(name) > MaxName || !utf8.ValidString(name) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", ErrName
	}
	return name, nil
}

// SetName names the site; "" goes back to Default.
func (s *Store) SetName(ctx context.Context, name string) error {
	name, err := CleanName(name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := db.SetSetting(ctx, s.db, keyName, name); err != nil {
		return err
	}
	s.name = name
	return nil
}

// SetIcon makes an uploaded image the icon: its middle square, side × side.
func (s *Store) SetIcon(ctx context.Context, r io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(r, MaxUpload+1))
	if err != nil {
		return err
	}
	icon, err := square(data)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := db.SetSetting(ctx, s.db, keyIcon, base64.StdEncoding.EncodeToString(icon)); err != nil {
		return err
	}
	s.set(s.name, icon)
	return nil
}

// ClearIcon goes back to the built-in icon.
func (s *Store) ClearIcon(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := db.SetSetting(ctx, s.db, keyIcon, ""); err != nil {
		return err
	}
	s.set(s.name, nil)
	return nil
}

// square decodes an upload and returns its middle square, resized to side, as PNG.
func square(data []byte) ([]byte, error) {
	if len(data) > MaxUpload {
		return nil, ErrLarge
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "png" && format != "jpeg" && format != "webp" {
		return nil, ErrFormat
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, ErrLarge
	}
	if min(cfg.Width, cfg.Height) < minSide {
		return nil, ErrSmall
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrFormat
	}
	b := src.Bounds()
	n := min(b.Dx(), b.Dy())
	from := image.Rect(0, 0, n, n).Add(b.Min).Add(image.Pt((b.Dx()-n)/2, (b.Dy()-n)/2))
	dst := image.NewRGBA(image.Rect(0, 0, side, side))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, from, draw.Src, nil)
	return encode(dst)
}

func encode(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// variant makes one of Files from the icon kept.
func variant(icon []byte, name string) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(icon))
	if err != nil {
		return nil, err
	}
	resized := func(n int) *image.RGBA {
		dst := image.NewRGBA(image.Rect(0, 0, n, n))
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
		return dst
	}
	switch name {
	case "icon-32.png":
		return encode(resized(32))
	case "icon-192.png":
		return encode(resized(192))
	case "icon-512.png":
		return icon, nil
	case "icon-180.png":
		dst := image.NewRGBA(image.Rect(0, 0, 180, 180))
		draw.Draw(dst, dst.Bounds(), image.NewUniform(backdrop(src)), image.Point{}, draw.Src)
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
		return encode(dst)
	case "icon-maskable.png":
		// The part no launcher cuts is the circle of 80% of the width: the picture fills that
		// much, its corners going a little past it, on its own edges' color.
		dst := image.NewRGBA(image.Rect(0, 0, side, side))
		draw.Draw(dst, dst.Bounds(), image.NewUniform(backdrop(src)), image.Point{}, draw.Src)
		inner := side * 8 / 10
		at := (side - inner) / 2
		draw.CatmullRom.Scale(dst, image.Rect(at, at, at+inner, at+inner), src, src.Bounds(), draw.Over, nil)
		return encode(dst)
	}
	return nil, fmt.Errorf("no icon file %q", name)
}

// backdrop is the color an icon is set on where it cannot be transparent: the average of its
// edges, white for an icon whose edges are mostly transparent.
func backdrop(img image.Image) color.Color {
	b := img.Bounds()
	var r, g, bl, a, n uint64
	add := func(x, y int) {
		cr, cg, cb, ca := img.At(x, y).RGBA() // premultiplied
		r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
	}
	for x := b.Min.X; x < b.Max.X; x++ {
		add(x, b.Min.Y)
		add(x, b.Max.Y-1)
	}
	for y := b.Min.Y + 1; y < b.Max.Y-1; y++ {
		add(b.Min.X, y)
		add(b.Max.X-1, y)
	}
	if n == 0 || a < n*0xffff/2 {
		return color.White
	}
	// Un-premultiply the average, as if the edges were set on nothing.
	return color.RGBA{uint8(r * 0xff / a), uint8(g * 0xff / a), uint8(bl * 0xff / a), 0xff}
}
