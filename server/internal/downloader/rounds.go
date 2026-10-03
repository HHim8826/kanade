package downloader

import (
	"path"
	"sort"
	"strings"
)

// Rounds (review #28). A selection larger than the staging budget is not refused: it downloads in
// rounds that each fit. A round is fetched, imported, and its files cleared once they are safely in
// the library; then the next round starts. Files of one folder go together when they fit; when a
// folder is too big for one round, its small companions (CUE sheets, logs, lyrics, a cover) come
// with its first round and stay on disk until the folder's last round, so every song is imported
// next to them.

const companionMax = 16 << 20

// companion is a small file that songs of its folder need at import: a sidecar or a cover.
func companion(f FileView) bool {
	ext := strings.ToLower(path.Ext(f.Path))
	base := strings.TrimSuffix(path.Base(f.Path), path.Ext(f.Path))
	return f.Length <= companionMax && (sidecarExt[ext] || (imageExt[ext] && coverLike.MatchString(base)))
}

// remaining are the selected files no round has taken yet.
func remaining(files []FileView) []FileView {
	var out []FileView
	for _, f := range files {
		if f.Selected && f.Round == 0 {
			out = append(out, f)
		}
	}
	return out
}

// Formats the importer converts to FLAC, and the compressed ones among them.
var (
	pcmExt       = map[string]bool{".wav": true, ".aif": true, ".aiff": true}
	losslessExt  = map[string]bool{".ape": true, ".tak": true, ".wv": true, ".tta": true}
	imageMinSize = int64(64 << 20)
)

// workFor estimates the staging space a file's import writes besides the file itself (review #46):
// the FLAC FFmpeg makes of a format players cannot all play, the songs cut from a disc image by a
// CUE sheet, the files unpacked from an archive. The importer holds the raw PCM size and a little
// more for FFmpeg output (ffmpeg.MaxFLAC), and outputs of a batch stay until its songs are in the
// library, so a round reserves the sum. Before the download only the size and name are known:
// uncompressed audio is its own size; compressed lossless audio, and a FLAC that is the disc image
// of a CUE sheet in its folder, are counted at 2.5 times their size.
func workFor(files []FileView, f FileView) int64 {
	const overhead = 256 << 10
	ext := strings.ToLower(path.Ext(f.Path))
	switch {
	case pcmExt[ext]:
		return f.Length + f.Length/64 + overhead
	case losslessExt[ext], ext == ".flac" && discImage(files, f):
		return f.Length*5/2 + 32*overhead // a split writes up to a few dozen songs
	case ext == ".zip":
		return 2 * f.Length
	}
	return 0
}

// discImage reports whether a large FLAC sits in a folder with a CUE sheet and no more audio
// files than sheets: each sheet then likely goes with one image that is cut into songs.
func discImage(files []FileView, f FileView) bool {
	if f.Length < imageMinSize {
		return false
	}
	dir := path.Dir(f.Path)
	cues, audio := 0, 0
	for _, o := range files {
		if !o.Selected || path.Dir(o.Path) != dir {
			continue
		}
		switch ext := strings.ToLower(path.Ext(o.Path)); {
		case ext == ".cue":
			cues++
		case audioExt[ext]:
			audio++
		}
	}
	return cues > 0 && audio <= cues
}

// planRound picks the next round's files within avail bytes, whole folders first-fit in path order;
// a file counts with the work space its import needs (workFor). When not even one folder fits, it
// takes as much of the first one as fits: its companions and its files in order; at least one file.
// bytes is what the round downloads, work the import's work space; alone reports a round larger
// than avail.
func planRound(files []FileView, avail int64) (pick []int, bytes, work int64, alone bool) {
	left := remaining(files)
	if len(left) == 0 {
		return nil, 0, 0, false
	}
	sort.Slice(left, func(i, j int) bool { return left[i].Path < left[j].Path })
	type folder struct {
		dir         string
		files       []FileView
		size, works int64
	}
	var dirs []*folder
	byDir := map[string]*folder{}
	for _, f := range left {
		d := path.Dir(f.Path)
		if byDir[d] == nil {
			byDir[d] = &folder{dir: d}
			dirs = append(dirs, byDir[d])
		}
		byDir[d].files = append(byDir[d].files, f)
		byDir[d].size += f.Length
		byDir[d].works += workFor(files, f)
	}
	for _, d := range dirs {
		if bytes+work+d.size+d.works <= avail {
			for _, f := range d.files {
				pick = append(pick, f.Index)
			}
			bytes += d.size
			work += d.works
		}
	}
	if len(pick) > 0 {
		return pick, bytes, work, false
	}
	// The first folder alone is too big: its companions, then its other files while they fit.
	d := dirs[0]
	for _, f := range d.files {
		if companion(f) {
			pick = append(pick, f.Index)
			bytes += f.Length
			work += workFor(files, f)
		}
	}
	took := false
	for _, f := range d.files {
		if companion(f) {
			continue
		}
		w := workFor(files, f)
		if !took || bytes+work+f.Length+w <= avail {
			pick = append(pick, f.Index)
			bytes += f.Length
			work += w
			took = true
		}
	}
	if !took && len(pick) == 0 { // nothing but companions left: they are small
		return nil, 0, 0, false
	}
	return pick, bytes, work, bytes+work > avail
}

// companionFor reports whether f is a companion fetched by an earlier round that this round's
// import needs: one of a folder that has files in the round, still on disk (review #45). The last
// round of a folder needs its CUE sheets as much as the first.
func companionFor(files []FileView, f FileView, round int) bool {
	if !f.Selected || f.Round == 0 || f.Round == round || !companion(f) {
		return false
	}
	dir := path.Dir(f.Path)
	for _, o := range files {
		if o.Selected && o.Round == round && path.Dir(o.Path) == dir {
			return true
		}
	}
	return false
}

// keepAfterRound reports whether a file of a finished round stays on disk for later rounds: a
// companion whose folder still has files to fetch.
func keepAfterRound(files []FileView, f FileView) bool {
	if !companion(f) {
		return false
	}
	dir := path.Dir(f.Path)
	for _, o := range files {
		if o.Selected && o.Round == 0 && path.Dir(o.Path) == dir {
			return true
		}
	}
	return false
}
