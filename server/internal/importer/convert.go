package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/ffmpeg"
	"github.com/HHim8826/kanade/server/internal/media"
)

// Conversion and CUE splitting (P2-4, decision D2 §2–3). Both happen during analysis, so the
// preview already shows the resulting songs; their output goes to staging/work and counts against
// the staging budget. Every result is checked: the decoded audio must be bit-identical (PCM MD5).

const (
	StateSplit      = "split" // a disc image cut into songs by its CUE sheet
	SourceConverted = "converted"
	SourceSplit     = "split"
)

// convertible are the lossless formats turned into FLAC; ALAC is m4a with codec alac. DSD and
// lossy formats other than the playable ones are skipped (D2 §2).
var convertible = map[string]bool{"wav": true, "aiff": true, "ape": true, "tak": true, "wavpack": true, "tta": true}

func needsConversion(info *media.Info) bool {
	return !info.Playable && (convertible[info.Format] || (info.Format == "m4a" && info.Codec == "alac"))
}

func hashFile(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func (im *Importer) itemFailed(ctx context.Context, id int64, state, msg string) {
	im.db.ExecContext(ctx, `UPDATE import_items SET state = ?, error = ?, updated_at = ? WHERE id = ?`, state, msg, db.Now(), id)
}

type audioItem struct {
	id        int64
	path, rel string
	info      media.Info
}

// pendingAudio lists the batch's audio still to import, with what was read of it.
func (im *Importer) pendingAudio(ctx context.Context, batchID int64) ([]audioItem, error) {
	rows, err := im.db.QueryContext(ctx, `SELECT id, local_path, rel_path, coalesce(info, '') FROM import_items
		WHERE batch_id = ? AND role = ? AND state = 'pending' AND source_kind = ''`, batchID, RoleAudio)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []audioItem
	for rows.Next() {
		var a audioItem
		var raw string
		if err := rows.Scan(&a.id, &a.path, &a.rel, &raw); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(raw), &a.info)
		out = append(out, a)
	}
	return out, rows.Err()
}

// splitCues cuts each disc image that a CUE sheet of the batch describes into songs.
func (im *Importer) splitCues(ctx context.Context, batchID int64) error {
	rows, err := im.db.QueryContext(ctx, `SELECT local_path, rel_path FROM import_items WHERE batch_id = ? AND role = ?
		AND state = 'pending' AND lower(rel_path) LIKE '%.cue'`, batchID, RoleSidecar)
	if err != nil {
		return err
	}
	type cueItem struct{ path, rel string }
	var cues []cueItem
	for rows.Next() {
		var c cueItem
		if err := rows.Scan(&c.path, &c.rel); err != nil {
			rows.Close()
			return err
		}
		cues = append(cues, c)
	}
	rows.Close()
	for _, c := range cues {
		audio, err := im.pendingAudio(ctx, batchID)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(c.path)
		if err != nil || len(data) > 1<<20 {
			continue
		}
		text, _ := media.DecodeText(data)
		sheet := parseCue(text)
		for _, f := range sheet.Files {
			if len(f.Tracks) < 2 {
				continue // a file per song: nothing to cut
			}
			img := resolveCueFile(path.Dir(c.rel), f.Name, audio, len(sheet.Files) == 1)
			if img == nil {
				continue
			}
			if err := im.splitImage(ctx, batchID, img, sheet, f); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveCueFile finds the audio a CUE FILE line means: the named file; else the same name with
// another extension (EAC writes .wav for what was saved as .flac); else, for a sheet with one FILE,
// the only audio in its folder.
func resolveCueFile(dir, name string, audio []audioItem, single bool) *audioItem {
	want := path.Join(dir, strings.ReplaceAll(name, `\`, "/"))
	stem := strings.TrimSuffix(want, path.Ext(want))
	var sameStem, inDir []*audioItem
	for i := range audio {
		a := &audio[i]
		if a.rel == want {
			return a
		}
		if strings.TrimSuffix(a.rel, path.Ext(a.rel)) == stem {
			sameStem = append(sameStem, a)
		}
		if path.Dir(a.rel) == dir {
			inDir = append(inDir, a)
		}
	}
	if len(sameStem) == 1 {
		return sameStem[0]
	}
	if single && len(inDir) == 1 {
		return inDir[0]
	}
	return nil
}

func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) || r < 32 {
			return '_'
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	return s
}

// splitImage cuts one image by its CUE sheet. The songs carry the sheet's names as tags and become
// items of their own; the image is marked split. A failure leaves the image failed, with the reason.
func (im *Importer) splitImage(ctx context.Context, batchID int64, img *audioItem, sheet *cueSheet, f cueFile) error {
	fail := func(msg string) error {
		im.itemFailed(ctx, img.id, StateFailed, msg)
		return nil
	}
	if im.FFmpeg == nil {
		return fail("splitting by the CUE sheet needs FFmpeg, which the server does not have")
	}
	s, err := im.FFmpeg.Probe(ctx, img.path)
	if err != nil {
		return fail("cannot read the disc image: " + err.Error())
	}
	if s.Float {
		return fail(ffmpeg.ErrFloat.Error())
	}
	pieces := f.pieces(s.SampleRate)
	if pieces == nil || (s.Samples > 0 && pieces[len(pieces)-1].Start >= s.Samples) || s.SampleRate%75 != 0 {
		return fail("the CUE sheet's track times do not fit the disc image")
	}
	sha, size, err := hashFile(img.path)
	if err != nil {
		return fail(err.Error())
	}
	if known, err := im.lib.SourceImported(ctx, sha, size); err != nil {
		return err
	} else if known {
		im.itemFailed(ctx, img.id, StateDuplicate, "this disc image was imported before")
		return nil
	}
	if im.Space != nil {
		if err := im.Space(ctx, size); err != nil {
			return fail("not enough staging space to split it: " + err.Error())
		}
	}
	dir := filepath.Join(im.workDir(batchID), "split", strconv.FormatInt(img.id, 10))
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	album := sheet.Title
	if album == "" {
		album = img.info.Tags.Album
	}
	if album == "" {
		album = strings.TrimSuffix(path.Base(img.rel), path.Ext(img.rel))
	}
	albumArtist := sheet.Performer
	if albumArtist == "" {
		albumArtist = img.info.Tags.AlbumArtist
	}
	date := sheet.Date
	if date == "" {
		date = img.info.Tags.Date
	}
	disc := sheet.Disc
	if disc == 0 {
		disc = img.info.Tags.DiscNo
	}
	cuts := make([]ffmpeg.Cut, len(pieces))
	var dsts []string
	for i, p := range pieces {
		title := p.Title
		if title == "" {
			title = fmt.Sprintf("Track %02d", p.Number)
		}
		artist := p.Performer
		if artist == "" {
			artist = albumArtist
		}
		dst := filepath.Join(dir, fmt.Sprintf("%02d %s.flac", p.Number, safeName(title)))
		tags := map[string]string{"title": title, "artist": artist, "album": album, "album_artist": albumArtist,
			"date": date, "genre": sheet.Genre, "track": strconv.Itoa(p.Number)}
		if disc > 0 {
			tags["disc"] = strconv.Itoa(disc)
		}
		cuts[i] = ffmpeg.Cut{Start: p.Start, End: p.End, Dst: dst, Tags: tags}
		dsts = append(dsts, dst)
	}
	if err := im.FFmpeg.Split(ctx, img.path, cuts, s); err != nil {
		os.RemoveAll(dir)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fail("splitting failed: " + err.Error())
	}
	// The songs put back together must be the image, sample for sample; a FLAC image's own checksum
	// must match too (a damaged download shows here).
	whole, err1 := im.FFmpeg.PCMMD5(ctx, s.Bits, img.path)
	joined, err2 := im.FFmpeg.PCMMD5(ctx, s.Bits, dsts...)
	switch {
	case ctx.Err() != nil:
		os.RemoveAll(dir)
		return ctx.Err()
	case err1 != nil || err2 != nil:
		os.RemoveAll(dir)
		return fail(fmt.Sprintf("cannot check the split: %v %v", err1, err2))
	case whole != joined:
		os.RemoveAll(dir)
		return fail("the split songs do not add up to the disc image; nothing was imported")
	case img.info.Format == "flac" && img.info.AudioMD5 != "" && strings.Trim(img.info.AudioMD5, "0") != "" && img.info.AudioMD5 != whole:
		os.RemoveAll(dir)
		return fail("the disc image does not match its own checksum: the file is damaged")
	}

	tx, err := im.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := db.Now()
	for _, d := range dsts {
		rel := path.Join(path.Dir(img.rel), filepath.Base(d))
		if _, err := tx.ExecContext(ctx, `INSERT INTO import_items (batch_id, local_path, rel_path, state, role, temp,
			source_path, source_kind, source_sha256, source_size, updated_at) VALUES (?, ?, ?, 'pending', ?, 1, ?, ?, ?, ?, ?)`,
			batchID, d, rel, RoleAudio, img.path, SourceSplit, sha, size, now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE import_items SET state = ?, error = ?, updated_at = ? WHERE id = ?`,
		StateSplit, fmt.Sprintf("split into %d songs by the CUE sheet", len(dsts)), now, img.id); err != nil {
		return err
	}
	return tx.Commit()
}

// convertAll turns the batch's convertible files into FLAC.
func (im *Importer) convertAll(ctx context.Context, batchID int64) error {
	audio, err := im.pendingAudio(ctx, batchID)
	if err != nil {
		return err
	}
	for _, a := range audio {
		if !needsConversion(&a.info) {
			continue
		}
		if err := im.convert(ctx, batchID, a); err != nil {
			return err
		}
	}
	return nil
}

func (im *Importer) convert(ctx context.Context, batchID int64, a audioItem) error {
	fail := func(msg string) error {
		im.itemFailed(ctx, a.id, StateFailed, msg)
		return nil
	}
	s, err := im.FFmpeg.Probe(ctx, a.path)
	if err != nil {
		return fail("cannot read audio: " + err.Error())
	}
	if s.Float {
		im.itemFailed(ctx, a.id, StateSkipped, ffmpeg.ErrFloat.Error())
		return nil
	}
	sha, size, err := hashFile(a.path)
	if err != nil {
		return fail(err.Error())
	}
	if known, err := im.lib.SourceImported(ctx, sha, size); err != nil {
		return err
	} else if known {
		im.itemFailed(ctx, a.id, StateDuplicate, "this file was converted and imported before")
		return nil
	}
	if im.Space != nil {
		if err := im.Space(ctx, size); err != nil {
			return fail("not enough staging space to convert it: " + err.Error())
		}
	}
	dir := filepath.Join(im.workDir(batchID), "conv", strconv.FormatInt(a.id, 10))
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(dir, strings.TrimSuffix(filepath.Base(a.path), filepath.Ext(a.path))+".flac")
	if err := im.FFmpeg.ToFLAC(ctx, a.path, dst, s); err != nil {
		os.RemoveAll(dir)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fail("conversion failed: " + err.Error())
	}
	src, err1 := im.FFmpeg.PCMMD5(ctx, s.Bits, a.path)
	out, err2 := im.FFmpeg.PCMMD5(ctx, s.Bits, dst)
	if err1 != nil || err2 != nil || src != out {
		os.RemoveAll(dir)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err1 != nil || err2 != nil {
			return fail(fmt.Sprintf("cannot check the conversion: %v %v", err1, err2))
		}
		return fail("the FLAC does not decode to the same audio; the original is kept")
	}
	// The FLAC is read again by analysis; the original stays where it is.
	_, err = im.db.ExecContext(ctx, `UPDATE import_items SET local_path = ?, temp = 1, source_path = ?, source_kind = ?,
		source_sha256 = ?, source_size = ?, info = NULL, updated_at = ? WHERE id = ?`,
		dst, a.path, SourceConverted, sha, size, db.Now(), a.id)
	return err
}

// sourceOf records, once a file made from a source is in the library, that the source is done.
func (im *Importer) sourceOf(ctx context.Context, it *item, assetID int64) {
	if it.sourceSHA == "" || assetID == 0 {
		return
	}
	if err := im.lib.AddSource(ctx, it.sourceSHA, it.sourceSize, assetID, it.sourceKind); err != nil {
		im.log.Warn("record import source", "item", it.id, "err", err)
	}
}

var errNoFFmpeg = errors.New("converting this format needs FFmpeg, which the server does not have")
