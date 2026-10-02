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

// planRound picks the next round's files within avail bytes, whole folders first-fit in path order.
// When not even one folder fits, it takes as much of the first one as fits: its companions and its
// files in order; at least one file. alone reports a single file larger than avail.
func planRound(files []FileView, avail int64) (pick []int, bytes int64, alone bool) {
	left := remaining(files)
	if len(left) == 0 {
		return nil, 0, false
	}
	sort.Slice(left, func(i, j int) bool { return left[i].Path < left[j].Path })
	type folder struct {
		dir   string
		files []FileView
		size  int64
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
	}
	for _, d := range dirs {
		if bytes+d.size <= avail {
			for _, f := range d.files {
				pick = append(pick, f.Index)
			}
			bytes += d.size
		}
	}
	if len(pick) > 0 {
		return pick, bytes, false
	}
	// The first folder alone is too big: its companions, then its other files while they fit.
	d := dirs[0]
	for _, f := range d.files {
		if companion(f) {
			pick = append(pick, f.Index)
			bytes += f.Length
		}
	}
	took := false
	for _, f := range d.files {
		if companion(f) {
			continue
		}
		if !took || bytes+f.Length <= avail {
			pick = append(pick, f.Index)
			bytes += f.Length
			took = true
		}
	}
	if !took && len(pick) == 0 { // nothing but companions left: they are small
		return nil, 0, false
	}
	return pick, bytes, bytes > avail
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
