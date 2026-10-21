package rarvol

import (
	"bytes"
	"crypto/rand"
	"errors"
	"sort"
	"testing"

	"github.com/kipsilabs/seedstrem/internal/downloader"
	"github.com/kipsilabs/seedstrem/internal/rarvol/rartest"
)

func TestVolumeNumber(t *testing.T) {
	cases := []struct {
		name string
		stem string
		n    int
		ok   bool
	}{
		{"Rel/rel.rar", "Rel/rel", 0, true},
		{"Rel/rel.r00", "Rel/rel", 1, true},
		{"Rel/rel.r99", "Rel/rel", 100, true},
		{"Rel/rel.s00", "Rel/rel", 101, true},
		{"Rel/REL.RAR", "Rel/REL", 0, true},
		{"rel.part01.rar", "rel", 0, true},
		{"rel.part1.rar", "rel", 0, true},
		{"rel.part12.rar", "rel", 11, true},
		{"rel.part00.rar", "", 0, false},
		{"rel.mkv", "", 0, false},
		{"rel.sfv", "", 0, false},
		{"rel.r0", "", 0, false},
	}
	for _, c := range cases {
		stem, n, ok := VolumeNumber(c.name)
		if stem != c.stem || n != c.n || ok != c.ok {
			t.Errorf("VolumeNumber(%q) = %q, %d, %v; want %q, %d, %v", c.name, stem, n, ok, c.stem, c.n, c.ok)
		}
	}
}

// torrentFiles lays volumes out the way a torrent lists them: sorted by
// name, which puts .r00… ahead of .rar.
func torrentFiles(vols []rartest.Volume, extra ...downloader.FileInfo) []downloader.FileInfo {
	var files []downloader.FileInfo
	for _, v := range vols {
		files = append(files, downloader.FileInfo{Name: "Rel/" + v.Name, Size: int64(len(v.Data))})
	}
	files = append(files, extra...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	for i := range files {
		files[i].Index = i
	}
	return files
}

func TestSetsOrdersVolumesByNumber(t *testing.T) {
	vols := rartest.Set("rel", "rel.mkv", make([]byte, 100), 30, rartest.Options{Format: 4})
	files := torrentFiles(vols, downloader.FileInfo{Name: "Rel/rel.nfo", Size: 1})
	sets := Sets(files)
	if len(sets) != 1 {
		t.Fatalf("got %d sets, want 1", len(sets))
	}
	var got []string
	for _, f := range sets[0].Files {
		got = append(got, f.Name)
	}
	want := []string{"Rel/rel.rar", "Rel/rel.r00", "Rel/rel.r01", "Rel/rel.r02"}
	if len(got) != len(want) {
		t.Fatalf("volumes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("volumes = %v, want %v", got, want)
		}
	}
	if first := sets[0].First(); first.Name != "Rel/rel.rar" || first.Index != 4 {
		t.Errorf("First() = %+v, want Rel/rel.rar at torrent index 4", first)
	}
	if s, ok := SetStartingAt(files, 4); !ok || s.Stem != "Rel/rel" {
		t.Errorf("SetStartingAt(4) = %+v, %v", s, ok)
	}
	if _, ok := SetStartingAt(files, 0); ok {
		t.Error("SetStartingAt on a middle volume should fail")
	}
}

func TestSetsDropsSetWithGap(t *testing.T) {
	files := []downloader.FileInfo{
		{Index: 0, Name: "a.r00"}, {Index: 1, Name: "a.r02"}, {Index: 2, Name: "a.rar"},
		{Index: 3, Name: "b.part1.rar"}, {Index: 4, Name: "b.part2.rar"},
	}
	sets := Sets(files)
	if len(sets) != 1 || sets[0].Stem != "b" {
		t.Fatalf("Sets = %+v, want only the gap-free set b", sets)
	}
}

func TestParse(t *testing.T) {
	data := []byte("0123456789")
	for _, format := range []int{4, 5} {
		for _, c := range []struct {
			name string
			o    rartest.Options
			err  error
		}{
			{"stored", rartest.Options{}, nil},
			{"compressed", rartest.Options{Compress: true}, ErrCompressed},
			{"encrypted", rartest.Options{Encrypt: true}, ErrEncrypted},
		} {
			c.o.Format = format
			vols := rartest.Set("rel", `Sub\rel.mkv`, data, 4, c.o)
			v := vols[0].Data
			h, err := Parse(bytes.NewReader(v), int64(len(v)))
			if !errors.Is(err, c.err) {
				t.Errorf("RAR%d %s: err = %v, want %v", format, c.name, err, c.err)
				continue
			}
			if err != nil {
				continue
			}
			if h.Format != format || h.InnerName != "Sub/rel.mkv" || h.PackSize != 4 || h.UnpackedSize != 10 {
				t.Errorf("RAR%d: header = %+v", format, h)
			}
			if got := string(v[h.DataOffset : h.DataOffset+h.PackSize]); got != "0123" {
				t.Errorf("RAR%d: data block = %q, want 0123", format, got)
			}
		}
	}
	if _, err := Parse(bytes.NewReader([]byte("not a rar file")), 14); !errors.Is(err, ErrNotRar) {
		t.Errorf("plain file: err = %v, want ErrNotRar", err)
	}
}

// parseVol parses one generated volume's headers.
func parseVol(t *testing.T, v rartest.Volume) Header {
	t.Helper()
	h, err := Parse(bytes.NewReader(v.Data), int64(len(v.Data)))
	if err != nil {
		t.Fatalf("parse %s: %v", v.Name, err)
	}
	return h
}

// buildFrom builds a set's layout from its first volume, as streaming
// does before any other volume is on disk.
func buildFrom(t *testing.T, vols []rartest.Volume) (Set, Layout) {
	t.Helper()
	sets := Sets(torrentFiles(vols))
	if len(sets) != 1 {
		t.Fatalf("got %d sets", len(sets))
	}
	l, err := Build(sets[0], parseVol(t, vols[0]))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return sets[0], l
}

func TestBuildReassemblesInnerFile(t *testing.T) {
	data := make([]byte, 1000)
	rand.Read(data)
	cases := []struct {
		name  string
		chunk int
		o     rartest.Options
	}{
		{"rar4 single volume", 2000, rartest.Options{Format: 4}},
		{"rar4 two volumes", 600, rartest.Options{Format: 4}},
		{"rar4 old naming", 90, rartest.Options{Format: 4}},
		{"rar4 new naming", 90, rartest.Options{Format: 4, NewNaming: true}},
		{"rar5", 90, rartest.Options{Format: 5, NewNaming: true}},
		// 200 volumes: the RAR5 volume number's vint grows to two bytes
		// from volume 128 on, shifting the predicted data offset.
		{"rar5 many volumes", 5, rartest.Options{Format: 5, NewNaming: true}},
		{"rar4 past .r99", 5, rartest.Options{Format: 4}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vols := rartest.Set("rel", "rel.mkv", data, c.chunk, c.o)
			set, l := buildFrom(t, vols)
			if l.Size != int64(len(data)) || l.InnerName != "rel.mkv" {
				t.Fatalf("layout size %d name %q, want %d rel.mkv", l.Size, l.InnerName, len(data))
			}
			var got []byte
			for i := range l.Segments {
				v := vols[i]
				if err := l.Resolve(i, parseVol(t, v), int64(len(v.Data))); err != nil {
					t.Fatalf("volume %d: %v", i, err)
				}
				seg := l.Segments[i]
				if !seg.Verified || seg.FileIndex != set.Files[i].Index {
					t.Fatalf("segment %d = %+v, want verified at file index %d", i, seg, set.Files[i].Index)
				}
				got = append(got, v.Data[seg.DataOffset:seg.DataOffset+seg.Length]...)
			}
			if !bytes.Equal(got, data) {
				t.Fatal("segments do not reassemble the inner file")
			}
		})
	}
}

func TestBuildRejectsVolumesThatDontFitTheFile(t *testing.T) {
	vols := rartest.Set("rel", "rel.mkv", make([]byte, 100), 30, rartest.Options{Format: 4})
	set := Sets(torrentFiles(vols))[0]
	h0 := parseVol(t, vols[0])
	h0.UnpackedSize = 1000 // more than the volumes can hold
	if _, err := Build(set, h0); !errors.Is(err, ErrLayout) {
		t.Fatalf("err = %v, want ErrLayout", err)
	}
}

func TestResolveRejectsMispredictedVolume(t *testing.T) {
	vols := rartest.Set("rel", "rel.mkv", make([]byte, 100), 30, rartest.Options{Format: 4})
	_, l := buildFrom(t, vols)
	h := parseVol(t, vols[1])
	h.DataOffset++
	if err := l.Resolve(1, h, int64(len(vols[1].Data))); !errors.Is(err, ErrLayout) {
		t.Fatalf("err = %v, want ErrLayout", err)
	}
	if l.Segments[1].Verified {
		t.Fatal("mismatched segment marked verified")
	}
}

func TestResolveAdoptsLastVolumeOffset(t *testing.T) {
	// RAR5's final part carries the file CRC, so its data starts later
	// than the first volume's headers predict.
	vols := rartest.Set("rel", "rel.mkv", make([]byte, 100), 30, rartest.Options{Format: 5, NewNaming: true})
	_, l := buildFrom(t, vols)
	last := len(vols) - 1
	h := parseVol(t, vols[last])
	if h.DataOffset == l.Segments[last].DataOffset {
		t.Fatal("test set should give the last volume longer headers")
	}
	if err := l.Resolve(last, h, int64(len(vols[last].Data))); err != nil {
		t.Fatal(err)
	}
	if l.Segments[last].DataOffset != h.DataOffset {
		t.Fatalf("last data offset %d, want %d", l.Segments[last].DataOffset, h.DataOffset)
	}
}
