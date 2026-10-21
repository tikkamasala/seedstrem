package stream

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kipsilabs/seedstrem/internal/config"
	"github.com/kipsilabs/seedstrem/internal/downloader"
	"github.com/kipsilabs/seedstrem/internal/downloader/fake"
	"github.com/kipsilabs/seedstrem/internal/playsession"
	"github.com/kipsilabs/seedstrem/internal/rarvol"
	"github.com/kipsilabs/seedstrem/internal/rarvol/rartest"
	"github.com/kipsilabs/seedstrem/internal/store"
	"github.com/kipsilabs/seedstrem/internal/torrents"
)

const (
	rarContentSize = 10_000
	rarChunk       = 3_000 // data bytes per volume: 4 volumes
)

// newRarEnv writes a stored RAR set holding a random rel.mkv to disk
// under Rel/, lists it in torrent (name) order — .r00… ahead of .rar —
// and links the .rar first volume. state gives every piece's initial
// state; progress every volume's.
func newRarEnv(t *testing.T, o rartest.Options, state int, progress float64) *streamEnv {
	t.Helper()
	dir := t.TempDir()
	content := make([]byte, rarContentSize)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	vols := rartest.Set("rel", "rel.mkv", content, rarChunk, o)
	if err := os.MkdirAll(filepath.Join(dir, "Rel"), 0o755); err != nil {
		t.Fatal(err)
	}
	var files []fake.File
	var total int64
	for _, v := range vols {
		if err := os.WriteFile(filepath.Join(dir, "Rel", v.Name), v.Data, 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, fake.File{Name: "Rel/" + v.Name, Size: int64(len(v.Data)), Priority: 1, Progress: progress})
		total += int64(len(v.Data))
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	first := -1
	for i, f := range files {
		if f.Name == "Rel/"+vols[0].Name {
			first = i
		}
	}

	states := make([]int, (total+pieceSize-1)/pieceSize)
	for i := range states {
		states[i] = state
	}
	f := fake.New()
	f.Put(&fake.Torrent{
		Hash: testHash, Name: "Rel", State: "downloading", SavePath: "/downloads",
		PieceSize: pieceSize, PieceStates: states, Files: files,
	})

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.InsertTorrent(ctx, store.Torrent{ID: "STREAMTEST000", Hash: testHash, Phase: store.PhaseSelected, AddedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertLinks(ctx, []store.Link{{Token: testToken, TorrentID: "STREAMTEST000", FileIndex: first, Path: files[first].Name, Bytes: total}}); err != nil {
		t.Fatal(err)
	}

	avail := NewAvailability(f)
	resolver := NewResolver(f, func() []config.Mapping {
		return []config.Mapping{{Remote: "/downloads", Local: dir}}
	})
	svc := torrents.New(st, f, func() torrents.Settings { return torrents.Settings{} }, nil)
	settings := func() Settings { return Settings{WaitTimeout: 5 * time.Second, ReadChunk: pieceSize} }
	h := NewHandler(st, f, svc, resolver, avail, playsession.New(), settings, nil)
	return &streamEnv{handler: h.Router(), h: h, fake: f, avail: avail, content: content, dir: dir}
}

func TestServeRarSetComplete(t *testing.T) {
	for _, o := range []rartest.Options{{Format: 4}, {Format: 4, NewNaming: true}, {Format: 5, NewNaming: true}} {
		t.Run(fmt.Sprintf("rar%d new=%v", o.Format, o.NewNaming), func(t *testing.T) {
			e := newRarEnv(t, o, 2, 1)

			w := e.get(t, "")
			if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), e.content) {
				t.Fatalf("full GET: status %d, %d bytes; want 200 with the %d-byte inner file", w.Code, w.Body.Len(), len(e.content))
			}

			// A range straddling the first volume boundary.
			w = e.get(t, fmt.Sprintf("bytes=%d-%d", rarChunk-10, rarChunk+9))
			if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), e.content[rarChunk-10:rarChunk+10]) {
				t.Fatalf("cross-volume range: status %d body %x", w.Code, w.Body.Bytes())
			}

			// The tail, where MKV cues live: it sits in the last volume.
			w = e.get(t, "bytes=-100")
			if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), e.content[len(e.content)-100:]) {
				t.Fatalf("suffix range: status %d", w.Code)
			}
			if got := w.Header().Get("Content-Range"); got != fmt.Sprintf("bytes %d-%d/%d", rarContentSize-100, rarContentSize-1, rarContentSize) {
				t.Errorf("Content-Range = %q", got)
			}
		})
	}
}

func TestServeRarSetWhileDownloading(t *testing.T) {
	e := newRarEnv(t, rartest.Options{Format: 4}, 0, 0)
	fakeClock(e, func() {
		e.fake.Update(testHash, func(tor *fake.Torrent) {
			// One more piece per poll — but only of wanted files, the way
			// a download client fetches: the window decides what arrives.
			// A file whose pieces are all in is complete.
			var off int64
			landed := false
			for i := range tor.Files {
				f := &tor.Files[i]
				first, last := int(off/pieceSize), int((off+f.Size-1)/pieceSize)
				off += f.Size
				have := 0
				for p := first; p <= last; p++ {
					if !landed && f.Priority > 0 && tor.PieceStates[p] != 2 {
						tor.PieceStates[p], landed = 2, true
					}
					if tor.PieceStates[p] == 2 {
						have++
					}
				}
				f.Progress = float64(have) / float64(last-first+1)
			}
		})
	})

	w := e.get(t, "")
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), e.content) {
		t.Fatalf("status %d, %d bytes; want the %d-byte inner file", w.Code, w.Body.Len(), len(e.content))
	}

	// Torrent order is r00, r01, r02, rar. Streaming from the .rar first
	// volume must unwant the others (they would download first), then
	// want each one as playback reaches it.
	var prio []string
	for _, c := range e.fake.Calls() {
		if strings.HasPrefix(c, "filePrio ") {
			prio = append(prio, strings.TrimPrefix(c, "filePrio hash="+testHash+" "))
		}
	}
	// The .rar completes while the gate waits on its headers (it is the
	// only wanted file), which lets .r00 in; entering .r00 reads ahead
	// into .r01, entering .r01 into .r02.
	want := []string{"indices=[0 1 2] priority=0", "indices=[0] priority=1", "indices=[1] priority=1", "indices=[2] priority=1"}
	if strings.Join(prio, "; ") != strings.Join(want, "; ") {
		t.Errorf("priority calls:\n  %v\nwant\n  %v", prio, want)
	}
}

func TestServeRarSetHeadersMissingServesPlaceholder(t *testing.T) {
	e := newRarEnv(t, rartest.Options{Format: 4}, 0, 0)
	fakeClock(e, nil)

	w := e.get(t, "")
	if want := placeholderFor(0); !bytes.Equal(w.Body.Bytes(), want) {
		t.Errorf("body = %d bytes, want the %d-byte downloading placeholder", w.Body.Len(), len(want))
	}
}

func TestServeCompressedRarSetIsUnsupported(t *testing.T) {
	e := newRarEnv(t, rartest.Options{Format: 5, Compress: true}, 2, 1)

	w := e.get(t, "")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415; body %s", w.Code, w.Body.String())
	}
}

func TestRarWindowReleasesWhenIdle(t *testing.T) {
	old := windowIdleGrace
	windowIdleGrace = 10 * time.Millisecond
	t.Cleanup(func() { windowIdleGrace = old })

	e := newRarEnv(t, rartest.Options{Format: 4}, 2, 0.5)
	if w := e.get(t, "bytes=0-99"); w.Code != http.StatusPartialContent {
		t.Fatalf("status = %d", w.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		allWanted := true
		for _, f := range e.fake.Get(testHash).Files {
			allWanted = allWanted && f.Priority > 0
		}
		if allWanted {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("volumes still unwanted after the window went idle: %+v", e.fake.Get(testHash).Files)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWindowWanted(t *testing.T) {
	// Torrent order r00, r01, rar: volume 0 is index 2.
	fi := func(i int, name string, progress float64) downloader.FileInfo {
		return downloader.FileInfo{Index: i, Name: name, Progress: progress}
	}
	set := rarvol.Set{Files: []downloader.FileInfo{fi(2, "a.rar", 0), fi(0, "a.r00", 0), fi(1, "a.r01", 0)}}
	cases := []struct {
		name   string
		files  []downloader.FileInfo
		active map[int]int
		want   map[int]bool
	}{
		{"idle wants everything", []downloader.FileInfo{fi(0, "", 0), fi(1, "", 0), fi(2, "", 0)}, nil,
			map[int]bool{0: true, 1: true, 2: true}},
		{"first volume alone: .r00 sorts ahead of it", []downloader.FileInfo{fi(0, "", 0), fi(1, "", 0), fi(2, "", 0.5)}, map[int]int{0: 1},
			map[int]bool{0: false, 1: false, 2: true}},
		{"first volume complete lets .r00 in", []downloader.FileInfo{fi(0, "", 0), fi(1, "", 0), fi(2, "", 1)}, map[int]int{0: 1},
			map[int]bool{0: true, 1: false, 2: true}},
		{"middle volume reads ahead", []downloader.FileInfo{fi(0, "", 0.2), fi(1, "", 0), fi(2, "", 1)}, map[int]int{1: 1},
			map[int]bool{0: true, 1: true, 2: true}},
		{"tail read wants only the last volume", []downloader.FileInfo{fi(0, "", 0), fi(1, "", 0), fi(2, "", 0)}, map[int]int{2: 1},
			map[int]bool{0: false, 1: true, 2: false}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := windowWanted(set, c.files, c.active)
			for idx, w := range c.want {
				if got[idx] != w {
					t.Errorf("file %d wanted = %v, want %v (all: %v)", idx, got[idx], w, got)
				}
			}
		})
	}
}
