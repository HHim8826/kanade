package downloader

import (
	"slices"
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
	pick, n, alone := planRound(files, 1000*mb)
	if !slices.Equal(pick, []int{1, 2, 3}) || n != 600*mb+1000 || alone {
		t.Fatalf("round 1: %v %d %v", pick, n, alone)
	}
	pick, _, _ = planRound(files, 1200*mb)
	if !slices.Equal(pick, []int{1, 2, 3, 4}) {
		t.Fatalf("two folders: %v", pick)
	}
	for _, i := range []int{0, 1, 2, 3} {
		files[i].Round = 1
	}
	// A folder bigger than the room: its cover first, then one file, larger than the room.
	pick, n, alone = planRound(files, 1000*mb)
	if !slices.Equal(pick, []int{6, 5}) || n != 3001*mb || !alone {
		t.Fatalf("too big: %v %d %v", pick, n, alone)
	}
	if keepAfterRound(files, files[2]) { // A is done: its cue goes
		t.Fatal("cue of a finished folder kept")
	}
	files[4].Round, files[5].Round = 2, 2
	if pick, _, _ := planRound(files, 1000*mb); pick != nil {
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
	pick, _, alone := planRound(files, 1000*mb)
	if !slices.Equal(pick, []int{4, 3, 1}) || alone {
		t.Fatalf("first part: %v %v", pick, alone)
	}
	for _, i := range []int{0, 2, 3} {
		files[i].Round = 1
	}
	if !keepAfterRound(files, files[2]) || !keepAfterRound(files, files[3]) || keepAfterRound(files, files[0]) {
		t.Fatal("companions not kept for the folder's next round")
	}
	pick, _, _ = planRound(files, 1000*mb)
	if !slices.Equal(pick, []int{2}) {
		t.Fatalf("second part: %v", pick)
	}
}
