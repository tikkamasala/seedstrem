package stream

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path"
	"time"

	"github.com/kipsilabs/seedstrem/internal/downloader"
	"github.com/kipsilabs/seedstrem/internal/rarvol"
	"github.com/kipsilabs/seedstrem/internal/store"
	"github.com/kipsilabs/seedstrem/internal/torrents"
)

// serveRar streams the video inside a stored RAR set (scene releases) as
// one file, straight off the volumes as they download. It reports
// whether the "still downloading" placeholder (or nothing, for a client
// already gone) was served instead, which serve keeps out of the
// abandoned-download cleanup.
func (h *Handler) serveRar(w http.ResponseWriter, r *http.Request, tor store.Torrent, files []downloader.FileInfo, set rarvol.Set, settings Settings) (servedPlaceholder bool) {
	ctx := r.Context()
	hash := tor.Hash
	vol0 := set.First()

	complete := true
	var have float64
	for _, f := range set.Files {
		complete = complete && f.Progress >= 1
		have += f.Progress * float64(f.Size)
	}
	progress := have / float64(max(set.TotalSize, 1))

	// The first volume holds the inner file's headers and size: keep it
	// in the download window for the whole serve, whatever this request
	// reads.
	h.window.enter(hash, set, 0)
	defer h.window.leave(hash, set, 0)

	var pieceSize int64
	if !complete {
		props, err := h.dc.Properties(ctx, hash)
		if err != nil || props.PieceSize <= 0 {
			h.retryLater(w, "piece size unavailable", err)
			return false
		}
		pieceSize = props.PieceSize

		// Playability gate on the first volume's headers only. The MKV
		// cues sit in the last volume, which the window wants only once a
		// read reaches it — a read waiting there is ordinary buffering,
		// and requiring the tail up front would pull the whole last
		// volume ahead of the first.
		first, last := PiecesForRange(FileOffset(files, vol0.Index), pieceSize, 0, min(vol0.Size, rarvol.HeaderProbe)-1)
		grace := readyGrace
		if r.Method == http.MethodHead {
			grace = 0
		}
		if !h.waitStreamReady(ctx, hash, first, last, first, last, grace) {
			if ctx.Err() != nil {
				h.logger.Debug("stream: client gone before rar headers were ready", "hash", hash, "error", ctx.Err())
				return true
			}
			h.logger.Info("stream: rar first volume headers not available, serving downloading placeholder",
				"hash", hash, "volume", vol0.Name, "progress", progress, "headPieces", [2]int{first, last})
			servePlaceholder(w, r, progress)
			return true
		}
	}

	path0, err := h.waitForFile(ctx, hash, vol0, settings.WaitTimeout)
	if err != nil {
		h.retryLater(w, "file not on disk yet", err)
		return false
	}
	h0, err := parseVolume(path0, vol0.Size)
	if err == nil && !torrents.IsVideo(h0.InnerName) {
		err = fmt.Errorf("archived file %q is not a video", h0.InnerName)
	}
	var layout rarvol.Layout
	if err == nil {
		layout, err = rarvol.Build(set, h0)
	}
	if err != nil {
		h.logger.Warn("stream: rar set cannot be streamed", "hash", hash, "volume", vol0.Name, "error", err)
		http.Error(w, "archive cannot be streamed: "+err.Error(), http.StatusUnsupportedMediaType)
		return false
	}

	contentType := mime.TypeByExtension(path.Ext(layout.InnerName))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	h.logger.Debug("stream: serving rar set",
		"hash", hash, "inner", layout.InnerName, "size", layout.Size, "volumes", len(set.Files), "complete", complete)

	chunk := settings.ReadChunk
	if chunk <= 0 {
		chunk = 4 << 20
	}
	blocked := &blockedRange{}
	waitFor := func(ctx context.Context, hash string, first, last int) error {
		return h.avail.WaitForRangeHint(ctx, hash, first, last, settings.WaitTimeout, prioRefreshInterval, func() bool {
			return h.prio.request(ctx, hash, first, last+readaheadPieces(pieceSize))
		})
	}
	open := func(i int) (*partialReader, error) {
		vol := set.Files[i]
		volOffset := FileOffset(files, vol.Index)
		volComplete := vol.Progress >= 1
		seg := &layout.Segments[i]
		if !seg.Verified && !volComplete {
			first, last := PiecesForRange(volOffset, pieceSize, 0, min(vol.Size, rarvol.HeaderProbe)-1)
			if err := waitFor(ctx, hash, first, last); err != nil {
				return nil, fmt.Errorf("waiting for volume headers: %w", err)
			}
		}
		p, err := h.waitForFile(ctx, hash, vol, settings.WaitTimeout)
		if err != nil {
			return nil, err
		}
		if !seg.Verified {
			hdr, err := parseVolume(p, vol.Size)
			if err == nil {
				err = layout.Resolve(i, hdr, vol.Size)
			}
			if err != nil {
				h.logger.Error("stream: rar volume does not match the set layout", "hash", hash, "volume", vol.Name, "error", err)
				return nil, err
			}
		}
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		return &partialReader{
			ctx:        ctx,
			file:       f,
			blocked:    blocked,
			size:       seg.Length,
			base:       seg.DataOffset,
			fileOffset: volOffset + seg.DataOffset,
			pieceSize:  pieceSize,
			hash:       hash,
			chunkSize:  chunk,
			complete:   volComplete,
			logger:     h.logger,
			reopen: func() (*os.File, error) {
				info, err := h.dc.Torrent(ctx, hash)
				if err != nil {
					return nil, err
				}
				p, err := h.resolver.FilePath(ctx, info, vol)
				if err != nil {
					return nil, err
				}
				return os.Open(p)
			},
			waitFor: waitFor,
		}, nil
	}
	rr := &rarReader{
		layout: &layout,
		open:   open,
		enter:  func(i int) { h.window.enter(hash, set, i) },
		leave:  func(i int) { h.window.leave(hash, set, i) },
	}
	defer rr.Close()

	stopBeat := make(chan struct{})
	if !complete {
		go h.heartbeat(ctx, stopBeat, hash, vol0.Index, blocked)
		// A volume completing can let the next one into the window; the
		// readers only move it when they cross a volume boundary.
		go func() {
			t := time.NewTicker(heartbeatInterval)
			defer t.Stop()
			for {
				select {
				case <-stopBeat:
					return
				case <-ctx.Done():
					return
				case <-t.C:
					h.window.refresh(hash, set)
				}
			}
		}()
	}
	serveStart := time.Now()
	http.ServeContent(w, r, path.Base(layout.InnerName), time.Time{}, rr)
	close(stopBeat)
	h.logger.Debug("stream: rar serve finished",
		"hash", hash, "bytesDelivered", rr.delivered, "elapsed", time.Since(serveStart).Round(time.Millisecond),
		"ctxErr", ctx.Err())
	return false
}

// parseVolume reads a RAR volume's headers from disk.
func parseVolume(p string, size int64) (rarvol.Header, error) {
	f, err := os.Open(p)
	if err != nil {
		return rarvol.Header{}, err
	}
	defer f.Close()
	return rarvol.Parse(f, size)
}
