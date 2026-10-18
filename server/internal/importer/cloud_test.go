package importer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HHim8826/kanade/server/internal/library"
	"github.com/HHim8826/kanade/server/internal/staging"
	"github.com/HHim8826/kanade/server/internal/storage"
	"github.com/HHim8826/kanade/server/internal/storage/storagetest"
)

// A bucket as a source (#100, and #95's way of importing): its songs read for their tags with
// ranged reads, fetched to staging one by one with their folder's cover and their .lrc, hashed
// there, and kept where they are; the copies go once imported, and nothing is written to the
// bucket. A song whose bytes change is imported again.
func TestCloudSource(t *testing.T) {
	ctx := context.Background()
	im, lib, fd := setup(t)
	set := withStores(t, im)
	fake := storagetest.NewS3(t)
	tmp := t.TempDir()
	song := func(name, title string) []byte {
		p := filepath.Join(tmp, name)
		taggedMP3(t, p, map[string]string{"TIT2": title, "TPE1": "Band", "TALB": "Cloud Album", "TRCK": "1"})
		b, _ := os.ReadFile(p)
		return b
	}
	one := song("one.mp3", "One")
	fake.Put("music/Cloud Album/Disc 1/01 One.mp3", one)
	fake.Put("music/Cloud Album/Disc 1/01 One.lrc", []byte("[00:01.00]la la"))
	fake.Put("music/Cloud Album/folder.jpg", jpeg)
	fake.Put("music/Cloud Album/notes.txt", []byte("x"))
	cfg, _ := json.Marshal(storage.S3Config{Endpoint: fake.URL, Bucket: fake.Bucket, Prefix: "music"})
	sec, _ := json.Marshal(storage.S3Secret{AccessKey: fake.AccessKey, SecretKey: fake.SecretKey})
	st, err := set.Add(ctx, storage.NewStore{Kind: storage.KindS3, Name: "R2", Mode: storage.Source, Config: cfg, Secret: sec})
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Join(fake.Names(), ",")
	res, err := im.ScanSource(ctx, set, st.ID, false)
	if err != nil || res.Files != 2 || res.New != 2 { // the song, and its folder's picture for the gallery (#134)
		t.Fatalf("scan %+v %v", res, err)
	}
	v := runUntilDone(t, im, res.Batch)
	if s := states(v)["Cloud Album/Disc 1/01 One.mp3"]; s != StatePublished {
		t.Fatalf("states %v", states(v))
	}
	if refs := assetRefs(t, im); refs["One"] != "verified s3:1:Cloud Album/Disc 1/01 One.mp3" {
		t.Fatalf("refs %v", refs)
	}
	if fd.uploads != 0 || strings.Join(fake.Names(), ",") != names || fake.Requests["PUT"]+fake.Requests["POST create"] != 0 {
		t.Fatalf("written: %d uploads to Drive, bucket %v", fd.uploads, fake.Names())
	}
	var sha string
	im.db.QueryRow(`SELECT sha256 FROM assets`).Scan(&sha)
	if want := sha256Hex(one); sha != want {
		t.Fatalf("the asset's SHA-256 %s, want %s (the file's own)", sha, want)
	}
	albums, _ := lib.Albums(ctx, 10, 0, false)
	if len(albums) != 1 || albums[0].CoverID == 0 {
		t.Fatalf("the folder's cover: %+v", albums)
	}
	if c, err := lib.Cover(ctx, albums[0].CoverID); err != nil || c.Ref != "s3:1:Cloud Album/folder.jpg" {
		t.Fatalf("the cover is not the bucket's picture, where it is: %+v %v", c, err)
	}
	var lyrics string
	im.db.QueryRow(`SELECT l.text FROM lyrics l JOIN tracks t ON t.id = l.track_id WHERE t.title = 'One'`).Scan(&lyrics)
	if !strings.Contains(lyrics, "la la") {
		t.Fatalf("lyrics %q", lyrics)
	}
	if left := tree(t, filepath.Join(im.staging, "work")); left != "" {
		t.Fatalf("copies left in staging:\n%s", left)
	}
	var got string
	if rc, err := set.Open(ctx, "s3:1:Cloud Album/Disc 1/01 One.mp3", 0, 3); err == nil {
		b := make([]byte, 4)
		rc.Read(b)
		rc.Close()
		got = string(b)
	}
	if got != string(one[:4]) {
		t.Fatalf("read from the bucket: %q", got)
	}

	// Other bytes in its place: the song is missing until imported again, then back.
	fake.Put("music/Cloud Album/Disc 1/01 One.mp3", append(song("one-b.mp3", "One"), 0)) // its tags alone may come out the same
	res, err = im.ScanSource(ctx, set, st.ID, false)
	if err != nil || res.Changed != 1 {
		t.Fatalf("changed: %+v %v", res, err)
	}
	runUntilDone(t, im, res.Batch)
	if refs := assetRefs(t, im); refs["One"] != "verified s3:1:Cloud Album/Disc 1/01 One.mp3" {
		t.Fatalf("after the change %v", refs)
	}
	// Gone: missing, the bucket being there.
	fake.Remove("music/Cloud Album/Disc 1/01 One.mp3")
	if res, err := im.ScanSource(ctx, set, st.ID, false); err != nil || res.Gone != 1 {
		t.Fatalf("gone: %+v %v", res, err)
	}
}

// A cloud source's pictures and lyrics hold staging space as the song does: one with no room left
// is left out and the song goes on; an .lrc too large to be lyrics is not fetched (review #217).
func TestCloudSourceExtrasHoldStaging(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	set := withStores(t, im)
	fake := storagetest.NewS3(t)
	song := func(title, n string) []byte {
		p := filepath.Join(t.TempDir(), "song.mp3")
		taggedMP3(t, p, map[string]string{"TIT2": title, "TPE1": "Band", "TALB": "Big Cover", "TRCK": n})
		b, _ := os.ReadFile(p)
		return b
	}
	one, two := song("One", "1"), song("Two", "2")
	big := append(append([]byte{}, jpeg...), make([]byte, 512<<10)...)
	fake.Put("music/Big Cover/01 One.mp3", one)
	fake.Put("music/Big Cover/01 One.lrc", []byte("[00:01.00]la la"))
	fake.Put("music/Big Cover/folder.jpg", big)
	fake.Put("music/Big Cover/02 Two.mp3", two)
	fake.Put("music/Big Cover/02 Two.lrc", append([]byte("[00:01.00]"), make([]byte, 300<<10)...))
	im.Budget = &staging.Budget{Limit: int64(max(len(one), len(two))) + 64<<10}
	im.Budget.Use(staging.OnDisk(im.WorkCommitted))
	var fetched atomic.Pointer[string]
	var pictureReads atomic.Int32
	fake.Fail = func(r *http.Request) int {
		switch name := path.Base(r.URL.Path); {
		case r.Method != http.MethodGet:
		case name == "folder.jpg":
			pictureReads.Add(1)
		case name == "02%20Two.lrc" || name == "02 Two.lrc":
			fetched.Store(&name)
		}
		return 0
	}
	var peak atomic.Int64
	im.Budget.Free = func(string) int64 { // free space as it stands, the peak of the work folder kept
		if n := im.WorkCommitted(ctx); n > peak.Load() {
			peak.Store(n)
		}
		return 1 << 40
	}
	cfg, _ := json.Marshal(storage.S3Config{Endpoint: fake.URL, Bucket: fake.Bucket, Prefix: "music"})
	sec, _ := json.Marshal(storage.S3Secret{AccessKey: fake.AccessKey, SecretKey: fake.SecretKey})
	st, err := set.Add(ctx, storage.NewStore{Kind: storage.KindS3, Name: "R2", Mode: storage.Source, Config: cfg, Secret: sec})
	if err != nil {
		t.Fatal(err)
	}
	res, err := im.ScanSource(ctx, set, st.ID, false)
	if err != nil || res.New != 3 { // the songs, and the picture for the gallery (#134)
		t.Fatalf("scan %+v %v", res, err)
	}
	v := runUntilDone(t, im, res.Batch)
	if s := states(v); s["Big Cover/01 One.mp3"] != StatePublished || s["Big Cover/02 Two.mp3"] != StatePublished {
		t.Fatalf("states %v", s)
	}
	if got := fetched.Load(); got != nil {
		t.Fatalf("fetched past the budget or the lyrics limit: %v", *got)
	}
	// The picture is read once, for the gallery: streamed, never fetched to staging beside a song.
	if n := pictureReads.Load(); n != 1 {
		t.Fatalf("the picture was read %d times", n)
	}
	if limit, _ := im.Budget.Limits(); peak.Load() > limit {
		t.Fatalf("the work folder held %d bytes, the budget %d", peak.Load(), limit)
	}
	albums, _ := lib.Albums(ctx, 10, 0, false)
	if len(albums) != 1 || albums[0].CoverID != 0 {
		t.Fatalf("albums %+v", albums)
	}
	var lyrics string
	im.db.QueryRow(`SELECT group_concat(l.text) FROM lyrics l JOIN tracks t ON t.id = l.track_id`).Scan(&lyrics)
	if lyrics != "[00:01.00]la la" {
		t.Fatalf("lyrics %q", lyrics)
	}
}

// Removing a source leaves nothing of it to import (review #222): a scan of it running refuses it;
// a batch of it in review is cancelled; the file being fetched is stopped and the files not imported
// yet are left out; the songs imported already leave with it, written once in the edit log (review
// #229); and no song is left pointing at it.
func TestRemoveSourceCallsOffImports(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	set := withStores(t, im)
	fake := storagetest.NewS3(t)
	tmp := t.TempDir()
	for i, title := range []string{"One", "Two", "Three"} {
		p := filepath.Join(tmp, title+".mp3")
		taggedMP3(t, p, map[string]string{"TIT2": title, "TPE1": "Band", "TALB": "Gone Album", "TRCK": fmt.Sprint(i + 1)})
		b, _ := os.ReadFile(p)
		fake.Put(fmt.Sprintf("music/Gone Album/%02d %s.mp3", i+1, title), b)
	}
	fetching, held := make(chan struct{}, 1), make(chan struct{})
	var fetched atomic.Int64
	fake.Fail = func(r *http.Request) int { // the second song fetched (whole, not read for its tags) waits
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, ".mp3") && r.Header.Get("Range") == "" && fetched.Add(1) == 2 {
			fetching <- struct{}{}
			select {
			case <-held:
			case <-r.Context().Done():
			}
		}
		return 0
	}
	defer close(held)
	add := func() *storage.Store {
		cfg, _ := json.Marshal(storage.S3Config{Endpoint: fake.URL, Bucket: fake.Bucket, Prefix: "music"})
		sec, _ := json.Marshal(storage.S3Secret{AccessKey: fake.AccessKey, SecretKey: fake.SecretKey})
		st, err := set.Add(ctx, storage.NewStore{Kind: storage.KindS3, Name: "R2", Mode: storage.Source, Config: cfg, Secret: sec})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	songs := -1
	remove := func(st *storage.Store) error {
		return im.RemoveSource(ctx, st.ID, storage.Ref(st.Kind, st.ID, ""), func(tx *sql.Tx) error {
			var err error
			if songs, err = lib.ForgetStore(ctx, tx, st.Kind, st.ID, st.Name); err != nil {
				return err
			}
			return set.RemoveTx(ctx, tx, st.ID)
		})
	}
	left := func() (tracks, refs int) {
		im.db.QueryRow(`SELECT count(*) FROM tracks`).Scan(&tracks)
		im.db.QueryRow(`SELECT (SELECT count(*) FROM assets WHERE file_ref LIKE 's3:%')
			+ (SELECT count(*) FROM covers WHERE file_ref LIKE 's3:%')`).Scan(&refs)
		return
	}

	// A scan running: refused.
	st := add()
	im.mu.Lock()
	im.scanning[st.ID] = true
	im.mu.Unlock()
	if err := remove(st); !errors.Is(err, ErrScanning) {
		t.Fatalf("while scanned: %v", err)
	}
	im.mu.Lock()
	delete(im.scanning, st.ID)
	im.mu.Unlock()

	// In review: the batch is cancelled.
	startWorker(t, im)
	res, err := im.ScanSource(ctx, set, st.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, im, res.Batch, BatchReview)
	if err := remove(st); err != nil {
		t.Fatal(err)
	}
	b, _ := im.Batch(ctx, res.Batch)
	if b.State != BatchCanceled || states(b)["Gone Album/01 One.mp3"] != StateExcluded {
		t.Fatalf("in review: %s %v", b.State, states(b))
	}

	// Being imported: one song is in, another being fetched.
	st = add()
	if res, err = im.ScanSource(ctx, set, st.ID, false); err != nil || res.New != 3 {
		t.Fatalf("scan %+v %v", res, err)
	}
	select {
	case <-fetching:
	case <-time.After(10 * time.Second):
		t.Fatal("the second song was not fetched")
	}
	var groups int
	im.db.QueryRow(`SELECT count(*) FROM edit_groups`).Scan(&groups)
	if err := remove(st); err != nil {
		t.Fatal(err)
	}
	waitState(t, im, res.Batch, BatchDone)
	b, _ = im.Batch(ctx, res.Batch)
	count := map[string]int{}
	for _, state := range states(b) {
		count[state]++
	}
	if count[StatePublished] != 1 || count[StateExcluded] != 2 || songs != 1 || fetched.Load() != 2 {
		t.Fatalf("being imported: %v, %d songs let go of, %d fetched", states(b), songs, fetched.Load())
	}
	if tracks, refs := left(); tracks != 0 || refs != 0 {
		t.Fatalf("%d songs and %d files left pointing at the source", tracks, refs)
	}
	var summaries []string
	rows, _ := im.db.Query(`SELECT summary FROM edit_groups ORDER BY id LIMIT -1 OFFSET ?`, groups)
	for rows.Next() {
		var s string
		rows.Scan(&s)
		summaries = append(summaries, s)
	}
	rows.Close()
	if fmt.Sprint(summaries) != "[移除儲存位置「R2」（1 首）]" {
		t.Fatalf("edit log %v", summaries)
	}
	// A file of it placed after all the same is refused.
	var asset int64
	im.db.QueryRow(`SELECT id FROM assets LIMIT 1`).Scan(&asset)
	if err := lib.PlaceAsset(ctx, asset, storage.Ref(st.Kind, st.ID, "Gone Album/03 Three.mp3")); !errors.Is(err, library.ErrStoreGone) {
		t.Fatalf("placed in a store gone: %v", err)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// A OneDrive as a source (#95): signed in, its folder chosen, its songs imported as a bucket's are;
// each fetched copy checked against OneDrive's quickXorHash (a damaged one fails, and is not taken
// for the file), and the SHA-256 Kanade's own. Nothing is written to the OneDrive.
func TestOneDriveSourceImport(t *testing.T) {
	ctx := context.Background()
	im, _, _ := setup(t)
	set := withStores(t, im)
	g := storagetest.NewGraph(t)
	g.QuickXor = func(b []byte) string {
		h := storage.NewQuickXorHash()
		h.Write(b)
		return base64.StdEncoding.EncodeToString(h.Sum(nil))
	}
	t.Cleanup(storage.UseMicrosoft(g.Login(), g.API()))
	tmp := t.TempDir()
	song := func(name, title string) []byte {
		p := filepath.Join(tmp, name)
		taggedMP3(t, p, map[string]string{"TIT2": title, "TPE1": "Band", "TALB": "OneDrive Album", "TRCK": "1"})
		b, _ := os.ReadFile(p)
		return b
	}
	one, two := song("one.mp3", "One"), song("two.mp3", "Two")
	music := g.Add("Music", nil)
	oneID := g.Add("Music/OneDrive Album/01 One.mp3", one)
	twoID := g.Add("Music/OneDrive Album/02 Two.mp3", two)
	g.Add("Music/OneDrive Album/cover.jpg", jpeg)
	cfg, _ := json.Marshal(storage.OneDriveConfig{ClientID: g.ClientID})
	sec, _ := json.Marshal(storage.OneDriveSecret{ClientSecret: g.ClientSecret})
	st, err := set.Add(ctx, storage.NewStore{Kind: storage.KindOneDrive, Name: "OneDrive", Mode: storage.Source, Config: cfg, Secret: sec})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := im.ScanSource(ctx, set, st.ID, false); err == nil {
		t.Fatal("scanned before signing in")
	}
	raw, _ := set.OneDriveAuthURL(ctx, st.ID, "http://localhost/cb")
	u, _ := url.Parse(raw)
	if _, err := set.OneDriveComplete(ctx, url.Values{"state": {u.Query().Get("state")}, "code": {g.Code}}, "http://localhost/cb"); err != nil {
		t.Fatal(err)
	}
	if _, err := set.SetFolder(ctx, st.ID, music); err != nil {
		t.Fatal(err)
	}
	g.Damage = func(id string) bool { return id == twoID }
	res, err := im.ScanSource(ctx, set, st.ID, false)
	if err != nil || res.Files != 3 || res.New != 3 { // the songs, and the cover for the gallery (#134)
		t.Fatalf("scan %+v %v", res, err)
	}
	v := runUntilDone(t, im, res.Batch)
	got := states(v)
	if got["OneDrive Album/01 One.mp3"] != StatePublished || got["OneDrive Album/02 Two.mp3"] != StateFailed {
		t.Fatalf("states %v", got)
	}
	for _, it := range v.Items {
		if it.RelPath == "OneDrive Album/02 Two.mp3" && !strings.Contains(it.Error, "checksum") {
			t.Fatalf("the damaged copy: %q", it.Error)
		}
	}
	if refs := assetRefs(t, im); refs["One"] != "verified onedrive:1:"+oneID || refs["Two"] != "" {
		t.Fatalf("refs %v", refs)
	}
	var sha string
	im.db.QueryRow(`SELECT sha256 FROM assets`).Scan(&sha)
	if sha != sha256Hex(one) {
		t.Fatalf("the asset's SHA-256 %s, want the file's own", sha)
	}
	if g.Requests["create session"]+g.Requests["upload"]+g.Requests["delete"] != 0 {
		t.Fatalf("written to the OneDrive: %v", g.Requests)
	}
	if left := tree(t, filepath.Join(im.staging, "work")); left != "" {
		t.Fatalf("copies left in staging:\n%s", left)
	}
	// Downloads mended: retried, the file is fetched from the OneDrive again. (A scan leaves a file
	// that failed and did not change since alone.)
	g.Damage = nil
	if res, err := im.ScanSource(ctx, set, st.ID, false); err != nil || res.New+res.Changed != 0 {
		t.Fatalf("scanned again: %+v %v", res, err)
	}
	if r, err := im.Retry(ctx, res.Batch); err != nil || r.Requeued != 1 || r.Lost != 0 {
		t.Fatalf("retry %+v %v", r, err)
	}
	runUntilDone(t, im, res.Batch)
	if refs := assetRefs(t, im); refs["Two"] != "verified onedrive:1:"+twoID {
		t.Fatalf("after the retry %v", refs)
	}
	var state string
	im.db.QueryRow(`SELECT state FROM source_files WHERE key = ?`, twoID).Scan(&state)
	if state != "imported" {
		t.Fatalf("source file %q", state)
	}
}

// A gallery's picture is told of as it is kept, its original a local file then (review #248): one
// in a folder where it is, one uploaded; a cloud source's scan (a TIFF) copied aside as it is read,
// once, and the copy gone after.
func TestPicturesToldAsKept(t *testing.T) {
	ctx := context.Background()
	im, lib, _ := setup(t)
	set := withStores(t, im)
	type told struct {
		path string
		same bool
	}
	var got []told
	tell := func(ctx context.Context, m library.AlbumImage, src string) {
		b, err := os.ReadFile(src)
		got = append(got, told{m.Path, err == nil && sha256Hex(b) == m.SHA256 && int64(len(b)) == m.Size})
	}
	im.Pictured = tell
	tmp := t.TempDir()
	mp3 := filepath.Join(tmp, "01.mp3")
	taggedMP3(t, mp3, map[string]string{"TIT2": "One", "TPE1": "Band", "TALB": "Cloud", "TRCK": "1"})
	song, _ := os.ReadFile(mp3)
	tiff := pictureIn(t, "tiff", 40, 30, color.RGBA{1, 2, 3, 255})
	fake := storagetest.NewS3(t)
	fake.Put("music/Cloud/01.mp3", song)
	fake.Put("music/Cloud/Scans/back.tif", tiff)
	fake.Put("music/Cloud/Scans/front.jpg", pictureIn(t, "jpeg", 8, 8, color.RGBA{9, 9, 9, 255}))
	// Wider than a browser is shown, in far less than 16 MB (review #248).
	fake.Put("music/Cloud/Scans/booklet.jpg", pictureIn(t, "jpeg", 6000, 8, color.RGBA{5, 5, 5, 255}))
	fake.Put("music/Cloud/Scans/obi.png", pictureIn(t, "png", 8, 5000, color.RGBA{6, 6, 6, 255}))
	fake.Put("music/Cloud/Scans/disc.webp", webpOf(4200, 4200))
	cfg, _ := json.Marshal(storage.S3Config{Endpoint: fake.URL, Bucket: fake.Bucket, Prefix: "music"})
	sec, _ := json.Marshal(storage.S3Secret{AccessKey: fake.AccessKey, SecretKey: fake.SecretKey})
	st, err := set.Add(ctx, storage.NewStore{Kind: storage.KindS3, Name: "R2", Mode: storage.Source, Config: cfg, Secret: sec})
	if err != nil {
		t.Fatal(err)
	}
	reads := map[string]int{}
	var mu sync.Mutex
	fake.Fail = func(r *http.Request) int {
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/Scans/") {
			mu.Lock()
			reads[path.Base(r.URL.Path)]++
			mu.Unlock()
		}
		return 0
	}
	res, err := im.ScanSource(ctx, set, st.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	runUntilDone(t, im, res.Batch)
	if len(gallery(t, lib, albumNamed(t, lib, "Cloud"))) != 5 {
		t.Fatal("the gallery")
	}
	// The TIFF and the large ones told of, each read once; the small JPEG is shown as it is, and
	// not copied.
	slices.SortFunc(got, func(a, b told) int { return strings.Compare(a.path, b.path) })
	if want := []told{{"Scans/back.tif", true}, {"Scans/booklet.jpg", true}, {"Scans/disc.webp", true}, {"Scans/obi.png", true}}; !slices.Equal(got, want) {
		t.Fatalf("told %+v", got)
	}
	for name, n := range reads {
		if n != 1 {
			t.Fatalf("%s read %d times", name, n)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(im.staging, "picture-*")); len(left) != 0 {
		t.Fatalf("copies left: %v", left)
	}

	// In a folder, where it is; uploaded.
	im, lib, _ = setup(t)
	im.Pictured = tell
	got = nil
	src := t.TempDir()
	taggedMP3(t, filepath.Join(src, "Local/01.mp3"), map[string]string{"TIT2": "Two", "TPE1": "Band", "TALB": "Local", "TRCK": "1"})
	put(t, filepath.Join(src, "Local/Scans/obi.tif"), tiff)
	batch, _, err := im.CreateBatch(ctx, "local", "", src, false)
	if err != nil {
		t.Fatal(err)
	}
	runUntilDone(t, im, batch)
	p := filepath.Join(t.TempDir(), "upload")
	put(t, p, pictureIn(t, "bmp", 9, 9, color.RGBA{4, 4, 4, 255}))
	if _, added, err := im.AddAlbumImage(ctx, albumNamed(t, lib, "Local"), "disc.bmp", p); err != nil || !added {
		t.Fatalf("upload: %v %v", added, err)
	}
	if len(got) != 2 || got[0] != (told{"Scans/obi.tif", true}) || got[1] != (told{"disc.bmp", true}) {
		t.Fatalf("told %+v", got)
	}
}

// webpOf is the start of a lossless WebP w×h: what tells its size (as much as reading it for a
// gallery looks at).
func webpOf(w, h int) []byte {
	bits := uint32(w-1) | uint32(h-1)<<14
	chunk := []byte{0x2f, byte(bits), byte(bits >> 8), byte(bits >> 16), byte(bits >> 24), 0}
	out := []byte("RIFF")
	out = binary.LittleEndian.AppendUint32(out, uint32(4+8+len(chunk)))
	out = append(out, "WEBPVP8L"...)
	out = binary.LittleEndian.AppendUint32(out, 5)
	return append(out, chunk...)
}
