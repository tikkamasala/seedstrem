package stream

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/kipsilabs/seedstrem/internal/downloader"
	"github.com/kipsilabs/seedstrem/internal/rarvol"
)

// windowIdleGrace is how long a RAR set's window outlives its last
// reader before every volume is wanted again. Players close and reopen
// connections constantly (see abandonGrace); flipping the whole set to
// wanted in those gaps would send the picker off to .r00. A var so tests
// can shrink it.
var windowIdleGrace = abandonGrace

// volumeWindow makes a torrent download a RAR set's volumes in volume
// order while it is streamed.
//
// File priorities alone can't: under sequential download libtorrent
// ranks only top-priority (7) pieces, every other wanted piece goes in
// piece order — and torrents list .r00… ahead of the .rar first volume.
// So the window works by *wanting*: only the volumes readers are in are
// wanted, plus the next one as readahead. While streaming, the rest of
// the set stays at 0. Once nobody has streamed for windowIdleGrace,
// every volume is wanted again so the set completes in the background.
type volumeWindow struct {
	dc     downloader.Client
	logger *slog.Logger
	// changed runs after priorities were rewritten (qBittorrent drops the
	// first/last-piece boost on a file-priority rewrite).
	changed func(ctx context.Context, hash string)

	mu   sync.Mutex
	sets map[windowKey]*windowSet

	applyMu sync.Mutex // serializes read-modify-write of file priorities
}

type windowSet struct {
	set    rarvol.Set
	active map[int]int // volume number -> readers in it
}

func newVolumeWindow(dc downloader.Client, logger *slog.Logger, changed func(ctx context.Context, hash string)) *volumeWindow {
	return &volumeWindow{dc: dc, logger: logger, changed: changed, sets: map[windowKey]*windowSet{}}
}

// windowKey identifies one set's window: a season pack can stream
// several episodes' sets from the same torrent at once.
type windowKey struct{ hash, stem string }

// enter registers a reader in volume vol and applies the window before
// returning, so the reader's piece wait targets wanted pieces.
func (w *volumeWindow) enter(hash string, set rarvol.Set, vol int) {
	key := windowKey{hash, set.Stem}
	w.mu.Lock()
	ws := w.sets[key]
	if ws == nil {
		ws = &windowSet{set: set, active: map[int]int{}}
		w.sets[key] = ws
	}
	ws.active[vol]++
	w.mu.Unlock()
	w.apply(key)
}

// leave unregisters a reader from volume vol.
func (w *volumeWindow) leave(hash string, set rarvol.Set, vol int) {
	key := windowKey{hash, set.Stem}
	w.mu.Lock()
	ws := w.sets[key]
	if ws == nil {
		w.mu.Unlock()
		return
	}
	if ws.active[vol]--; ws.active[vol] <= 0 {
		delete(ws.active, vol)
	}
	idle := len(ws.active) == 0
	w.mu.Unlock()
	if idle {
		time.AfterFunc(windowIdleGrace, func() { w.releaseIfIdle(key) })
		return
	}
	w.apply(key)
}

// refresh re-applies the window: a volume completing can let its
// successor in (see windowWanted).
func (w *volumeWindow) refresh(hash string, set rarvol.Set) { w.apply(windowKey{hash, set.Stem}) }

// releaseIfIdle drops the window of a set nobody streams any more and
// wants every volume again.
func (w *volumeWindow) releaseIfIdle(key windowKey) {
	w.mu.Lock()
	ws := w.sets[key]
	if ws == nil || len(ws.active) > 0 {
		w.mu.Unlock()
		return
	}
	delete(w.sets, key)
	w.mu.Unlock()
	w.applyWanted(key.hash, ws.set, nil)
}

func (w *volumeWindow) apply(key windowKey) {
	w.mu.Lock()
	ws := w.sets[key]
	if ws == nil || len(ws.active) == 0 {
		// Idle: releaseIfIdle wants the whole set once the grace is up.
		w.mu.Unlock()
		return
	}
	set := ws.set
	active := maps.Clone(ws.active)
	w.mu.Unlock()
	w.applyWanted(key.hash, set, active)
}

// applyWanted sets the set's volumes wanted (priority 1) or not (0) per
// windowWanted, touching only volumes whose state changes. Wanted volumes
// keep whatever non-zero priority they already have. Best-effort: a
// failure costs ordering, not playback, so it is logged.
func (w *volumeWindow) applyWanted(hash string, set rarvol.Set, active map[int]int) {
	w.applyMu.Lock()
	defer w.applyMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	files, err := w.dc.Files(ctx, hash)
	if err != nil {
		w.logger.Debug("stream: rar window: read files", "hash", hash, "error", err)
		return
	}
	want := windowWanted(set, files, active)
	var on, off []int
	for _, f := range files {
		wanted, inSet := want[f.Index]
		switch {
		case !inSet:
		case wanted && f.Priority == 0:
			on = append(on, f.Index)
		case !wanted && f.Priority != 0:
			off = append(off, f.Index)
		}
	}
	if len(on) == 0 && len(off) == 0 {
		return
	}
	if len(off) > 0 {
		if err := w.dc.SetFilePriority(ctx, hash, off, 0); err != nil {
			w.logger.Warn("stream: rar window: unwant volumes", "hash", hash, "error", err)
			return
		}
	}
	if len(on) > 0 {
		if err := w.dc.SetFilePriority(ctx, hash, on, 1); err != nil {
			w.logger.Warn("stream: rar window: want volumes", "hash", hash, "error", err)
			return
		}
	}
	w.logger.Debug("stream: rar window moved", "hash", hash, "active", active, "wanted", on, "unwanted", off)
	if w.changed != nil {
		w.changed(ctx, hash)
	}
}

// windowWanted decides, per torrent file index, whether each volume of
// set is wanted. With no active readers every volume is. Otherwise the
// wanted volumes are those a reader is in, those already complete (so
// they keep seeding), and the one after each active volume — unless the
// torrent lists that one first (the .rar → .r00 step): sequential
// download would then fetch it ahead of the volume being read, so it
// only joins once that volume is complete.
func windowWanted(set rarvol.Set, files []downloader.FileInfo, active map[int]int) map[int]bool {
	progress := make(map[int]float64, len(files))
	for _, f := range files {
		progress[f.Index] = f.Progress
	}
	want := make(map[int]bool, len(set.Files))
	for v, f := range set.Files {
		want[f.Index] = len(active) == 0 || active[v] > 0 || progress[f.Index] >= 1
	}
	for v := range active {
		if v+1 >= len(set.Files) {
			continue
		}
		cur, next := set.Files[v], set.Files[v+1]
		if next.Index > cur.Index || progress[cur.Index] >= 1 {
			want[next.Index] = true
		}
	}
	return want
}
