// Package rarvol streams the single file inside a stored (uncompressed)
// multi-volume RAR set — the way scene releases ship video — as one
// virtual file. A stored archive keeps the file's bytes verbatim, split
// across the volumes' data blocks, so the virtual file is just those
// blocks concatenated in volume order: no extraction needed.
package rarvol

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kipsilabs/seedstrem/internal/downloader"
)

var (
	// x.part01.rar / x.part1.rar (new-style naming, RAR3+ and RAR5)
	reNewStyle = regexp.MustCompile(`(?i)^(.+)\.part(\d{1,4})\.rar$`)
	// x.rar, x.r00…x.r99, x.s00… (old-style scene naming)
	reOldStyle = regexp.MustCompile(`(?i)^(.+)\.(rar|[r-z]\d{2})$`)
)

// VolumeNumber reports the 0-based volume number of a RAR volume name and
// the stem shared by every volume of its set. Old-style sets start at
// x.rar, continue with x.r00…x.r99 and then x.s00…; new-style sets count
// x.part01.rar upwards.
func VolumeNumber(name string) (stem string, n int, ok bool) {
	if m := reNewStyle.FindStringSubmatch(name); m != nil {
		p, _ := strconv.Atoi(m[2])
		if p < 1 {
			return "", 0, false
		}
		return m[1], p - 1, true
	}
	if m := reOldStyle.FindStringSubmatch(name); m != nil {
		ext := strings.ToLower(m[2])
		if ext == "rar" {
			return m[1], 0, true
		}
		d, _ := strconv.Atoi(ext[1:])
		return m[1], 1 + int(ext[0]-'r')*100 + d, true
	}
	return "", 0, false
}

// Set is one complete RAR volume set inside a torrent.
type Set struct {
	Stem      string
	Files     []downloader.FileInfo // in volume order: Files[0] is the first volume
	TotalSize int64
}

// First returns the set's first volume.
func (s Set) First() downloader.FileInfo { return s.Files[0] }

// Sets groups a torrent's files into RAR volume sets, ordered by stem.
// Sets with a gap in their volume numbering are dropped: a missing volume
// would leave a hole in the virtual file.
func Sets(files []downloader.FileInfo) []Set {
	type vol struct {
		n int
		f downloader.FileInfo
	}
	byStem := map[string][]vol{}
	for _, f := range files {
		if stem, n, ok := VolumeNumber(f.Name); ok {
			byStem[stem] = append(byStem[stem], vol{n, f})
		}
	}
	var sets []Set
	for stem, vols := range byStem {
		sort.Slice(vols, func(i, j int) bool { return vols[i].n < vols[j].n })
		set := Set{Stem: stem}
		complete := true
		for i, v := range vols {
			if v.n != i {
				complete = false
				break
			}
			set.Files = append(set.Files, v.f)
			set.TotalSize += v.f.Size
		}
		if complete {
			sets = append(sets, set)
		}
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].Stem < sets[j].Stem })
	return sets
}

// SetStartingAt returns the set whose first volume is the torrent file
// with the given index.
func SetStartingAt(files []downloader.FileInfo, fileIndex int) (Set, bool) {
	for _, s := range Sets(files) {
		if s.First().Index == fileIndex {
			return s, true
		}
	}
	return Set{}, false
}
