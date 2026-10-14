package importer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	_ "image/gif" // what a picture can be, read for its size
	_ "image/jpeg"
	_ "image/png"
	"io"
	"maps"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	_ "golang.org/x/image/webp" // TIFF and BMP are measured here (tiffSize, bmpSize)

	"github.com/HHim8826/kanade/server/internal/db"
	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/storage"
)

// An album's gallery (#134): the pictures in its folder and in the folders below it (Scans/, BK/,
// Booklet/, Artwork/ ...), kept as they are with the album like its CUE sheets and logs, after the
// batch's songs. A picture finds its album by the nearest folder above it that an album was imported
// from, in its batch or in an earlier one from the same place (a later round of a download, a
// source's later scan: a source's albums get their pictures by the next scan). The preview can leave
// an album's pictures out.

// RoleImage is a picture for an album's gallery.
const RoleImage = "image"

const maxImage = 128 << 20

var (
	bookletExt = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".tif": true, ".tiff": true, ".bmp": true}
	// copies of the cover other players leave (Windows Media Player's), and macOS's resource forks
	junkImage = regexp.MustCompile(`(?i)^(albumart(small|_\{[0-9a-f-]+\}_(large|small))|\._.*)$`)
	imageMIME = map[string]string{"jpeg": "image/jpeg", "png": "image/png", "gif": "image/gif", "webp": "image/webp", "tiff": "image/tiff",
		"bmp": "image/bmp"}
	// what a picture is, said by its name or its folder's, in this order (a back cover is a cover too)
	imageKinds = []struct {
		kind string
		re   *regexp.Regexp
	}{
		{"back", regexp.MustCompile(`(?i)back|rear|tray|inlay|裏|背`)},
		{"obi", regexp.MustCompile(`(?i)(^|[^a-z])obi([^a-z]|$)|帯`)},
		{"disc", regexp.MustCompile(`(?i)(^|[^a-z])(disc|disk|cd|label)\s*\d*([^a-z]|$)|盤`)},
		{"front", regexp.MustCompile(`(?i)front|cover|folder|jacket|表紙|封面`)},
		{"booklet", regexp.MustCompile(`(?i)(^|[^a-z])(booklet|book|bk|page|inner|insert|lyrics?)([^a-z]|$)|(^|[^a-z])p\d+|歌詞|ブックレット`)},
	}
)

func isImage(name string) bool { return bookletExt[strings.ToLower(path.Ext(name))] }

// imageKind guesses what a picture is from its name, then its folder's.
func imageKind(rel string) string {
	for _, s := range []string{stem(rel), path.Base(path.Dir(rel))} {
		for _, k := range imageKinds {
			if k.re.MatchString(s) {
				return k.kind
			}
		}
	}
	return ""
}

// imageDisc is the disc of the folder a picture is in, below the album's (Disc 2/Scans/01.jpg): 0
// for none.
func imageDisc(inAlbum string) int {
	for _, d := range strings.Split(path.Dir(inAlbum), "/") {
		if m := discDir.FindStringSubmatch(d); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}

func (im *Importer) processImage(ctx context.Context, it *item) (outcome, error) {
	var out outcome
	if junkImage.MatchString(stem(it.rel)) {
		out.state, out.msg = StateSkipped, "a copy of the cover another player made, not a scan"
		return out, nil
	}
	if it.driveSize > maxImage {
		out.state, out.msg = StateSkipped, ErrNotImage.Error()
		return out, nil
	}
	var rc io.ReadCloser
	var err error
	switch files, ok := im.files(); {
	case it.path != "":
		rc, err = os.Open(it.path)
	case it.ref != "" && ok: // in a cloud source: read as it is streamed, never fetched to staging
		rc, err = files.Open(ctx, it.ref, 0, -1)
	case it.driveID != "": // in the Drive inbox: read here, then moved where it goes
		if it.path, err = im.fetchDrive(ctx, it.batchID, it.id, it.driveID, it.rel, it.driveSize); err == nil {
			it.temp = true
			rc, err = os.Open(it.path)
		}
	default:
		err = errors.New("storage locations are not available")
	}
	if err != nil {
		return out, err
	}
	m, err := readImage(rc)
	rc.Close()
	if errors.Is(err, ErrNotImage) {
		out.state, out.msg = StateSkipped, err.Error()
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.sha = m.SHA256
	albumID, folder, err := im.imageAlbum(ctx, it.batchID, it.rel)
	if err != nil {
		return out, err
	}
	if albumID == 0 {
		out.state, out.msg = StateSkipped, "no album was imported from this folder"
		return out, nil
	}
	m.AlbumID, m.Path = albumID, it.rel
	if folder != "." {
		m.Path = strings.TrimPrefix(it.rel, folder+"/")
	}
	m.Kind, m.Disc = imageKind(m.Path), imageDisc(m.Path)
	if it.ref != "" { // in a source (#99): kept where it is, in place of what was there (review #238)
		m.Ref = it.ref
		added, err := im.lib.PlaceImage(ctx, m)
		if out.state = StatePublished; !added {
			out.state = StateDuplicate // taken out of the gallery too: it stays out
		}
		return out, err
	}
	if have, err := im.lib.FindImage(ctx, albumID, m.SHA256, m.Size); err != nil || have != nil {
		out.state = StateDuplicate // taken out of the gallery too: it stays out
		return out, err
	}
	switch df, ok := im.df(); {
	case ok && it.driveID != "": // from the Drive inbox: move it, do not upload a copy
		dir, err := im.imageDir(ctx, m)
		if err != nil {
			return out, err
		}
		parent, err := im.drive.Folder(ctx, dir)
		if err != nil {
			return out, err
		}
		if err := df.Move(ctx, it.driveID, parent, []string{it.driveParent}); err != nil {
			return out, err
		}
		m.Ref = it.driveID
	default:
		if _, err := im.keepImage(ctx, m, it.path); err != nil {
			return out, err
		}
		out.state = StatePublished
		return out, nil
	}
	if _, err := im.lib.AddImage(ctx, m); err != nil {
		return out, err
	}
	out.state = StatePublished
	return out, nil
}

// ErrNotImage is a file that is no picture for a gallery: not one this can read, or too large.
var ErrNotImage = errors.New("not a JPEG, PNG, WebP, TIFF or BMP picture of at most 128 MB")

// readImage reads a picture as it comes: its type, size and SHA-256.
func readImage(r io.Reader) (library.AlbumImage, error) {
	var m library.AlbumImage
	h := sha256.New()
	n := &counter{}
	br := bufio.NewReader(io.TeeReader(io.LimitReader(r, maxImage+1), io.MultiWriter(h, n)))
	var cfg image.Config
	var format string
	var err error
	switch magic, _ := br.Peek(4); {
	case string(magic) == "II*\x00" || string(magic) == "MM\x00*":
		cfg, err = tiffSize(br)
		format = "tiff"
	case len(magic) == 4 && string(magic[:2]) == "BM":
		cfg, err = bmpSize(br)
		format = "bmp"
	default:
		cfg, format, err = image.DecodeConfig(br)
	}
	if m.MIME = imageMIME[format]; err != nil || m.MIME == "" || !bookletExt[mimeExt[m.MIME]] {
		return m, ErrNotImage
	}
	if _, err := io.Copy(io.Discard, br); err != nil {
		return m, err
	}
	if n.n > maxImage {
		return m, ErrNotImage
	}
	m.SHA256, m.Size, m.Width, m.Height = hex.EncodeToString(h.Sum(nil)), n.n, cfg.Width, cfg.Height
	return m, nil
}

// tiffSize reads a TIFF's size from its first directory, as it comes. A scan Go cannot decode
// (CMYK, YCbCr, JPEG inside) is kept all the same, and shown by FFmpeg's copy; and the whole file
// is not held in memory to find a directory written after the picture, as Go's decoder would.
func tiffSize(br *bufio.Reader) (image.Config, error) {
	var head [8]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		return image.Config{}, ErrNotImage
	}
	var bo binary.ByteOrder = binary.LittleEndian
	if head[0] == 'M' {
		bo = binary.BigEndian
	}
	off := int64(bo.Uint32(head[4:]))
	if off < 8 || off > maxImage {
		return image.Config{}, ErrNotImage
	}
	if _, err := br.Discard(int(off - 8)); err != nil {
		return image.Config{}, ErrNotImage
	}
	var count [2]byte
	if _, err := io.ReadFull(br, count[:]); err != nil {
		return image.Config{}, ErrNotImage
	}
	entries := make([]byte, 12*int(bo.Uint16(count[:])))
	if _, err := io.ReadFull(br, entries); err != nil {
		return image.Config{}, ErrNotImage
	}
	var cfg image.Config
	for e := entries; len(e) >= 12; e = e[12:] {
		var v int
		switch bo.Uint16(e[2:]) { // the value's type: a count of 1 is in the entry itself
		case 3: // SHORT
			v = int(bo.Uint16(e[8:]))
		case 4: // LONG
			v = int(bo.Uint32(e[8:]))
		default:
			continue
		}
		switch bo.Uint16(e) {
		case 256: // ImageWidth
			cfg.Width = v
		case 257: // ImageLength
			cfg.Height = v
		}
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return image.Config{}, ErrNotImage
	}
	return cfg, nil
}

// bmpSize reads a BMP's size from its header: one Go cannot decode (RLE, a bit field) is kept all
// the same.
func bmpSize(br *bufio.Reader) (image.Config, error) {
	var head [26]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		return image.Config{}, ErrNotImage
	}
	var cfg image.Config
	switch dib := binary.LittleEndian.Uint32(head[14:]); {
	case dib == 12: // BITMAPCOREHEADER
		cfg.Width, cfg.Height = int(binary.LittleEndian.Uint16(head[18:])), int(binary.LittleEndian.Uint16(head[20:]))
	case dib >= 40 && dib <= 124:
		w, h := int32(binary.LittleEndian.Uint32(head[18:])), int32(binary.LittleEndian.Uint32(head[22:]))
		cfg.Width, cfg.Height = int(w), int(max(h, -h)) // a negative height is top down
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return image.Config{}, ErrNotImage
	}
	return cfg, nil
}

// counter counts what is written to it.
type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

var mimeExt = map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/tiff": ".tif", "image/bmp": ".bmp"}

// imageDir is the folder a picture of an album's gallery is kept in, in a managed store: images/,
// then the album's folder as for its songs, then the folders it was in below the album's.
func (im *Importer) imageDir(ctx context.Context, m library.AlbumImage) (string, error) {
	album, err := im.lib.Album(ctx, m.AlbumID)
	if err != nil {
		return "", err
	}
	if album == nil {
		return "", errors.New("the album is gone")
	}
	return HomeDir("image", album.AlbumArtist, album.Title, path.Dir(m.Path)), nil
}

// HomeDir is the folder a managed location keeps an album's file in, as an import puts it: the
// file's kind's folder (library/ for a song, sidecars/, images/), then the album artist and the
// album; for a picture, then the folders it was in below the album's (sub). A cover is in covers/.
func HomeDir(kind, albumArtist, album, sub string) string {
	dir := libraryDir(library.EntryInput{Album: album, AlbumArtist: albumArtist})
	switch kind {
	case "cover":
		return "covers"
	case "sidecar":
		return "sidecars/" + strings.TrimPrefix(dir, "library/")
	case "image":
		dir = "images/" + strings.TrimPrefix(dir, "library/")
		if sub != "." && sub != "" {
			for _, s := range strings.Split(sub, "/") {
				if s = strings.TrimSpace(unsafeChars.Replace(s)); s != "" && s != "." && s != ".." {
					dir += "/" + s
				}
			}
		}
	}
	return dir
}

// imageFile is the picture at p as it is kept in the managed store, where imageDir says.
func (im *Importer) imageFile(ctx context.Context, m library.AlbumImage, p string) (storage.PutFile, error) {
	dir, err := im.imageDir(ctx, m)
	if err != nil {
		return storage.PutFile{}, err
	}
	name := strings.TrimSpace(unsafeChars.Replace(path.Base(m.Path)))
	if name == "" || name == "." || name == ".." {
		name = m.SHA256[:16] + mimeExt[m.MIME]
	}
	return storage.PutFile{Path: p, Dir: dir, Name: name, MIME: m.MIME, Size: m.Size, SHA256: m.SHA256, Sessions: &memSessions{}}, nil
}

// keepImage uploads the picture at p to the managed store and adds it to its album's gallery: m
// with its ID and file.
func (im *Importer) keepImage(ctx context.Context, m library.AlbumImage, p string) (library.AlbumImage, error) {
	f, err := im.imageFile(ctx, m, p)
	if err != nil {
		return m, err
	}
	err = im.keep(ctx, 0, f, func(ref string, since int64) (err error) {
		m.Ref = ref
		m.ID, err = im.lib.KeepImage(ctx, m, since)
		return err
	})
	return m, err
}

// AddAlbumImage keeps a picture uploaded for album albumID's gallery, named name, from the file at
// p (#134): one there already is listed as it is, and one taken out before is put back. It reports
// the picture, and whether it was added (or put back).
func (im *Importer) AddAlbumImage(ctx context.Context, albumID int64, name, p string) (*library.AlbumImage, bool, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, false, err
	}
	m, err := readImage(f)
	f.Close()
	if err != nil {
		return nil, false, err
	}
	if albumID, _, err = im.lib.AlbumNow(ctx, albumID); err != nil {
		return nil, false, err
	}
	if albumID == 0 {
		return nil, false, library.ErrNotFound
	}
	have, err := im.lib.FindImage(ctx, albumID, m.SHA256, m.Size)
	if err != nil {
		return nil, false, err
	}
	m.AlbumID, m.Path = albumID, path.Base(strings.ReplaceAll(name, "\\", "/"))
	if m.Path == "" || m.Path == "." || m.Path == "/" {
		m.Path = m.SHA256[:16] + mimeExt[m.MIME]
	}
	m.Name, m.Kind = m.Path, imageKind(m.Path)
	if have != nil {
		if _, hidden, err := im.lib.Image(ctx, have.ID); err != nil || !hidden {
			return have, false, err
		}
		f, err := im.imageFile(ctx, *have, p)
		if err != nil {
			return nil, false, err
		}
		if err := im.keep(ctx, 0, f, func(ref string, since int64) error {
			have.Ref = ref
			return im.lib.ShowImage(ctx, have.ID, ref, since)
		}); err != nil {
			return nil, false, err
		}
		return have, true, nil
	}
	if m, err = im.keepImage(ctx, m, p); err != nil {
		return nil, false, err
	}
	return &m, true, nil
}

// imageAlbum is the album a picture at rel goes with, as it is now (merged since: the album it went
// into), and the folder it was imported from: the nearest folder above the picture that an album was.
func (im *Importer) imageAlbum(ctx context.Context, batchID int64, rel string) (int64, string, error) {
	for d := path.Dir(rel); ; d = path.Dir(d) {
		id, err := im.folderAlbum(ctx, batchID, d)
		if err != nil || id != 0 {
			if id != 0 {
				id, _, err = im.lib.AlbumNow(ctx, id)
			}
			return id, d, err
		}
		if d == "." || d == "/" {
			return 0, "", nil
		}
	}
}

// folderAlbum is the album imported from folder (relative to the batch's root), in this batch or,
// for want of one, the latest from the same place: the same root and kind, and the same source.
func (im *Importer) folderAlbum(ctx context.Context, batchID int64, folder string) (int64, error) {
	key := coverKey{batchID, folder}
	im.mu.Lock()
	id, ok := im.folderAlbums[key]
	im.mu.Unlock()
	if ok {
		return id, nil
	}
	err := im.db.QueryRowContext(ctx, `SELECT e.album_id FROM import_items i JOIN album_entries e ON e.id = i.entry_id
		WHERE i.batch_id = ? AND i.role = ? AND json_extract(i.plan, '$.folder') = ? ORDER BY i.id LIMIT 1`,
		batchID, RoleAudio, folder).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		err = im.db.QueryRowContext(ctx, `SELECT e.album_id FROM import_batches me JOIN import_batches b ON b.root = me.root AND b.kind = me.kind
			AND coalesce(json_extract(b.options, '$.store'), 0) = coalesce(json_extract(me.options, '$.store'), 0) AND b.id != me.id
			JOIN import_items i ON i.batch_id = b.id JOIN album_entries e ON e.id = i.entry_id
			WHERE me.id = ? AND (me.root != '' OR json_extract(me.options, '$.store') IS NOT NULL)
			AND i.role = ? AND json_extract(i.plan, '$.folder') = ? ORDER BY i.id DESC LIMIT 1`, batchID, RoleAudio, folder).Scan(&id)
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return 0, err
	}
	im.mu.Lock()
	im.folderAlbums[key] = id
	im.mu.Unlock()
	return id, nil
}

// batchImage is a picture of a batch, as the preview sees it.
type batchImage struct {
	id         int64
	rel, state string
}

func (im *Importer) batchImages(ctx context.Context, batchID int64) ([]batchImage, error) {
	rows, err := im.db.QueryContext(ctx, `SELECT id, rel_path, state FROM import_items WHERE batch_id = ? AND role = ?`, batchID, RoleImage)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []batchImage
	for rows.Next() {
		var m batchImage
		if err := rows.Scan(&m.id, &m.rel, &m.state); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// imageGroup is the group a picture at rel goes with in a batch's plan: that of the nearest folder
// above it whose songs are in a group (folders: folder -> group); "" for none.
func imageGroup(rel string, folders map[string]string) string {
	for d := path.Dir(rel); ; d = path.Dir(d) {
		if g, ok := folders[d]; ok {
			return g
		}
		if d == "." || d == "/" {
			return ""
		}
	}
}

// groupFolders maps the folders of a batch's groups (group -> its songs' folders) to their group: a
// folder with songs of two goes with the first, in the groups' order.
func groupFolders(byGroup map[string][]string) map[string]string {
	folders := map[string]string{}
	for _, k := range sortedGroups(slices.Collect(maps.Keys(byGroup))) {
		for _, f := range byGroup[k] {
			if _, ok := folders[f]; !ok {
				folders[f] = k
			}
		}
	}
	return folders
}

// setGroupImages leaves out (or takes again) the pictures that would go with a group's album.
func (im *Importer) setGroupImages(ctx context.Context, batchID int64, group string, keep bool) error {
	plans, err := im.loadPlans(ctx, batchID)
	if err != nil {
		return err
	}
	byGroup := map[string][]string{}
	for _, p := range plans {
		if p.plan.Group != "" {
			byGroup[p.plan.Group] = append(byGroup[p.plan.Group], p.plan.Folder)
		}
	}
	if byGroup[group] == nil {
		return badOp("no album %q in this import", group)
	}
	folders := groupFolders(byGroup)
	list, err := im.batchImages(ctx, batchID)
	if err != nil {
		return err
	}
	from, to := "pending", StateExcluded
	if keep {
		from, to = StateExcluded, "pending"
	}
	for _, m := range list {
		if m.state != from || imageGroup(m.rel, folders) != group {
			continue
		}
		if _, err := im.db.ExecContext(ctx, `UPDATE import_items SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
			to, db.Now(), m.id, from); err != nil {
			return err
		}
	}
	return nil
}
