package importer

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/HHim8826/kanade/server/internal/media"
)

// ZIP limits (plan §4): an archive that breaks one is refused as a whole, before anything is
// extracted, with the reason.
const (
	zipMaxEntries = 5000
	zipMaxRatio   = 100     // music hardly compresses; a higher ratio means a ZIP bomb
	zipMaxFile    = 4 << 30 // one file
)

// zipWanted is what an import uses from an archive: audio, sidecars, lyrics and pictures.
func zipWanted(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return audioExt[ext] || sidecarExt[ext] || ext == ".lrc" || imageExt[ext]
}

// zipName decodes an entry name: UTF-8 when it is, otherwise the D2 guess (CP932, then CP1252),
// which covers archives made on Japanese Windows without the UTF-8 flag.
func zipName(f *zip.File) string {
	name, _ := media.DecodeLegacy([]byte(f.Name))
	return strings.ReplaceAll(name, `\`, "/")
}

// expandZip checks an archive and extracts what an import uses under dir, returning the extracted
// files relative to dir. Nothing is extracted when a check fails.
func (im *Importer) expandZip(ctx context.Context, zipPath, dir string) ([]string, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("not a readable ZIP: %w", err)
	}
	defer r.Close()
	if len(r.File) > zipMaxEntries {
		return nil, fmt.Errorf("the ZIP has %d entries; the limit is %d", len(r.File), zipMaxEntries)
	}
	type entry struct {
		f    *zip.File
		name string
	}
	var want []entry
	var total int64
	for _, f := range r.File {
		name := zipName(f)
		if f.FileInfo().IsDir() || strings.HasPrefix(name, "__MACOSX/") || strings.HasPrefix(path.Base(name), "._") {
			continue
		}
		clean := path.Clean(name)
		if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, ":") {
			return nil, fmt.Errorf("a file in the ZIP points outside it: %s", name)
		}
		if !zipWanted(clean) {
			continue
		}
		size := f.UncompressedSize64
		if size > zipMaxFile {
			return nil, fmt.Errorf("%s is larger than %d GB", clean, zipMaxFile>>30)
		}
		if size > 0 && (f.CompressedSize64 == 0 || size/f.CompressedSize64 > zipMaxRatio) {
			return nil, fmt.Errorf("%s expands more than %d times: not a music file", clean, zipMaxRatio)
		}
		total += int64(size)
		want = append(want, entry{f, clean})
	}
	if len(want) == 0 {
		return nil, errors.New("the ZIP has no audio, cue, log, lyrics or image files")
	}
	if im.Space != nil {
		if err := im.Space(ctx, total); err != nil {
			return nil, fmt.Errorf("the ZIP expands to %d MB: %w", total>>20, err)
		}
	}
	var out []string
	for _, e := range want {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		dst := filepath.Join(dir, filepath.FromSlash(e.name))
		if rel, err := filepath.Rel(dir, dst); err != nil || strings.HasPrefix(rel, "..") {
			return out, fmt.Errorf("a file in the ZIP points outside it: %s", e.name)
		}
		if err := extract(e.f, dst); err != nil {
			return out, fmt.Errorf("%s: %w", e.name, err)
		}
		out = append(out, e.name)
	}
	return out, nil
}

// extract copies one entry, refusing more bytes than its header declared; archive/zip checks the CRC.
func extract(f *zip.File, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(rc, int64(f.UncompressedSize64)+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != int64(f.UncompressedSize64) {
		err = errors.New("the entry is not the size its header says")
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}
