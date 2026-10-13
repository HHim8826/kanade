package importer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

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
	if err != nil || res.Files != 1 || res.New != 1 {
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
	fake.Fail = func(r *http.Request) int {
		if name := path.Base(r.URL.Path); r.Method == http.MethodGet && (name == "folder.jpg" || name == "02%20Two.lrc" || name == "02 Two.lrc") {
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
	if err != nil || res.New != 2 {
		t.Fatalf("scan %+v %v", res, err)
	}
	v := runUntilDone(t, im, res.Batch)
	if s := states(v); s["Big Cover/01 One.mp3"] != StatePublished || s["Big Cover/02 Two.mp3"] != StatePublished {
		t.Fatalf("states %v", s)
	}
	if got := fetched.Load(); got != nil {
		t.Fatalf("fetched past the budget or the lyrics limit: %v", *got)
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
	if err != nil || res.Files != 2 || res.New != 2 {
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
