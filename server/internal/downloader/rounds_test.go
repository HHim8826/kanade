package downloader

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPlanRound(t *testing.T) {
	mb := int64(1 << 20)
	files := []FileView{
		{Index: 1, Path: "A/01.flac", Length: 300 * mb, Selected: true},
		{Index: 2, Path: "A/02.flac", Length: 300 * mb, Selected: true},
		{Index: 3, Path: "A/a.cue", Length: 1000, Selected: true},
		{Index: 4, Path: "B/01.flac", Length: 500 * mb, Selected: true},
		{Index: 5, Path: "C/big.flac", Length: 3000 * mb, Selected: true},
		{Index: 6, Path: "C/cover.jpg", Length: mb, Selected: true},
		{Index: 7, Path: "D/x.flac", Length: 100 * mb, Selected: false},
	}
	// Whole folders that fit, first fit.
	pick, n, _, alone := planRound(files, 1000*mb)
	if !slices.Equal(pick, []int{1, 2, 3}) || n != 600*mb+1000 || alone {
		t.Fatalf("round 1: %v %d %v", pick, n, alone)
	}
	pick, _, _, _ = planRound(files, 1200*mb)
	if !slices.Equal(pick, []int{1, 2, 3, 4}) {
		t.Fatalf("two folders: %v", pick)
	}
	for _, i := range []int{0, 1, 2, 3} {
		files[i].Round = 1
	}
	// A folder bigger than the room: its cover first, then one file, larger than the room.
	pick, n, _, alone = planRound(files, 1000*mb)
	if !slices.Equal(pick, []int{6, 5}) || n != 3001*mb || !alone {
		t.Fatalf("too big: %v %d %v", pick, n, alone)
	}
	if keepAfterRound(files, files[2]) { // A is done: its cue goes
		t.Fatal("cue of a finished folder kept")
	}
	files[4].Round, files[5].Round = 2, 2
	if pick, _, _, _ := planRound(files, 1000*mb); pick != nil {
		t.Fatalf("nothing left: %v", pick)
	}
}

func TestSplitFolderKeepsCompanions(t *testing.T) {
	mb := int64(1 << 20)
	files := []FileView{
		{Index: 1, Path: "A/01.flac", Length: 600 * mb, Selected: true},
		{Index: 2, Path: "A/02.flac", Length: 600 * mb, Selected: true},
		{Index: 3, Path: "A/rip.log", Length: 2000, Selected: true},
		{Index: 4, Path: "A/folder.jpg", Length: mb, Selected: true},
	}
	pick, _, _, alone := planRound(files, 1000*mb)
	if !slices.Equal(pick, []int{4, 3, 1}) || alone {
		t.Fatalf("first part: %v %v", pick, alone)
	}
	for _, i := range []int{0, 2, 3} {
		files[i].Round = 1
	}
	if !keepAfterRound(files, files[2]) || !keepAfterRound(files, files[3]) || keepAfterRound(files, files[0]) {
		t.Fatal("companions not kept for the folder's next round")
	}
	pick, _, _, _ = planRound(files, 1000*mb)
	if !slices.Equal(pick, []int{2}) {
		t.Fatalf("second part: %v", pick)
	}
}

// A round leaves room for what its import writes: two WAV files that each fit with their FLAC
// conversion go in separate rounds (review #46).
func TestRoundLeavesRoomForConversion(t *testing.T) {
	files := []FileView{
		{Index: 1, Path: "A/01.wav", Length: 264678, Selected: true},
		{Index: 2, Path: "A/02.wav", Length: 264678, Selected: true},
	}
	pick, n, work, alone := planRound(files, 1<<20)
	if len(pick) != 1 || n != 264678 || work < 264678 || n+work > 1<<20 || alone {
		t.Fatalf("round: %v %d %d %v", pick, n, work, alone)
	}
	// A disc image with its CUE sheet counts the songs cut from it; plain FLAC files count nothing.
	mb := int64(1 << 20)
	disc := []FileView{
		{Index: 1, Path: "D/disc.flac", Length: 300 * mb, Selected: true},
		{Index: 2, Path: "D/disc.cue", Length: 2000, Selected: true},
		{Index: 3, Path: "E/01.flac", Length: 300 * mb, Selected: true},
		{Index: 4, Path: "E/02.flac", Length: 300 * mb, Selected: true},
		{Index: 5, Path: "E/e.cue", Length: 2000, Selected: true},
		{Index: 6, Path: "F/x.ape", Length: 100 * mb, Selected: true},
	}
	if w := workFor(disc, disc[0]); w < 600*mb {
		t.Fatalf("disc image work %d", w)
	}
	if workFor(disc, disc[2]) != 0 || workFor(disc, disc[1]) != 0 {
		t.Fatal("tracks of a folder with a CUE counted as images")
	}
	if workFor(disc, disc[5]) < 200*mb {
		t.Fatal("APE without work space")
	}
}

// The last round of a folder imports the CUE sheet an earlier round fetched (review #45).
func TestCompanionForLastRound(t *testing.T) {
	files := []FileView{
		{Index: 1, Path: "Album/disc.cue", Length: 1000, Selected: true, Round: 1, Batch: 7},
		{Index: 2, Path: "Album/01 other.flac", Length: 10, Selected: true, Round: 1, Batch: 7},
		{Index: 3, Path: "Album/02 image.flac", Length: 10, Selected: true, Round: 2},
		{Index: 4, Path: "Other/o.cue", Length: 1000, Selected: true, Round: 1, Batch: 7},
	}
	if !companionFor(files, files[0], 2) {
		t.Fatal("the last round leaves out its folder's CUE")
	}
	if companionFor(files, files[3], 2) || companionFor(files, files[1], 2) {
		t.Fatal("a companion of another folder, or a song, taken along")
	}
	r := &row{dir: t.TempDir(), files: files}
	r.Round = 2
	for _, f := range files {
		p := filepath.Join(r.dir, filepath.FromSlash(f.Path))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	got := r.roundPaths()
	if len(got) != 2 || !strings.HasSuffix(got[0], "disc.cue") || !strings.HasSuffix(got[1], "02 image.flac") {
		t.Fatalf("paths %v", got)
	}
	if r.handedOver() {
		t.Fatal("handed over before round 2 has an import")
	}
	r.files[2].Batch = 8
	if !r.handedOver() {
		t.Fatal("not handed over")
	}
}

func TestInfoHash(t *testing.T) {
	info := "d6:lengthi5e4:name3:abc12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaae"
	torrent := []byte("d8:announce3:url4:info" + info + "e")
	sum := sha1.Sum([]byte(info))
	if h, err := infoHash(torrent); err != nil || h != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash %s %v", h, err)
	}
	for _, bad := range []string{"", "d4:info", "d4:infoi1", "d4:infod99:xe", "l1:ae", "d8:announce3:urle"} {
		if _, err := infoHash([]byte(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
