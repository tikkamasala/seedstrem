package stremio

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/kipsilabs/seedstrem/internal/metainfo"
	"github.com/kipsilabs/seedstrem/internal/prowlarr"
	"github.com/kipsilabs/seedstrem/internal/torrents"
)

// play handles GET|HEAD /play/{infohash} — the resolve half. It adds the
// magnet to qBittorrent, waits for metadata, selects the matching file,
// and 302-redirects to the /dl streaming endpoint.
func (h *Handler) play(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := h.settings()
	infohash := strings.ToLower(chi.URLParam(r, "infohash"))
	magnet := r.URL.Query().Get("magnet")

	// A release whose .torrent fetch was deferred out of search has no
	// hash yet: the path carries a download-link key (see playKey). Fetch
	// the .torrent now — only for the release actually played.
	var torrentFile []byte
	if r.URL.Query().Get("deferred") == "1" {
		var ok bool
		infohash, magnet, torrentFile, ok = h.fetchByKey(ctx, s, infohash)
		if !ok {
			http.Error(w, "stream expired, reload the stream list", http.StatusGone)
			return
		}
	}

	if magnet == "" {
		http.Error(w, "missing magnet", http.StatusBadRequest)
		return
	}
	// Guard: the magnet must match the path infohash.
	if hash, _, err := metainfo.FromMagnet(magnet); err != nil || hash != infohash {
		http.Error(w, "magnet does not match infohash", http.StatusBadRequest)
		return
	}

	sel := torrents.Selector{
		// Content identity (empty when the stream URL predates it) — used
		// only to persist what this torrent was added for, so later stream
		// requests can surface it as already-owned.
		Source:     r.URL.Query().Get("src"),
		ContentRef: r.URL.Query().Get("cid"),
		// Prowlarr indexer this release came from (empty for stream URLs
		// that predate it) — persisted so cleanup can apply the indexer's
		// own seed time.
		Indexer: r.URL.Query().Get("ix"),
	}
	if r.URL.Query().Get("series") == "1" {
		sel.IsSeries = true
		sel.Season, _ = strconv.Atoi(r.URL.Query().Get("s"))
		sel.Episode, _ = strconv.Atoi(r.URL.Query().Get("e"))
	}
	h.logger.Debug("stremio: play resolve",
		"infohash", infohash, "series", sel.IsSeries, "season", sel.Season, "episode", sel.Episode)

	// If we cached the raw .torrent for this infohash while building the
	// stream list, add it directly (skips qBittorrent's metadata fetch);
	// otherwise fall back to the magnet.
	if torrentFile == nil {
		torrentFile = h.torrentFiles.get(infohash)
	}
	if torrentFile == nil && !prowlarr.MagnetHasTrackers(magnet) {
		magnet, torrentFile = h.fetchDeferredTorrent(ctx, s, infohash, magnet)
	}
	link, err := h.svc.Resolve(ctx, magnet, torrentFile, sel)
	if err != nil {
		switch {
		case errors.Is(err, torrents.ErrMetadataTimeout):
			w.Header().Set("Retry-After", "5")
			http.Error(w, "torrent metadata not ready yet", http.StatusServiceUnavailable)
		case errors.Is(err, torrents.ErrNoFileMatch):
			http.Error(w, "no matching file in torrent", http.StatusNotFound)
		default:
			h.logger.Error("stremio: resolve failed", "infohash", infohash, "error", err)
			http.Error(w, "failed to resolve stream", http.StatusBadGateway)
		}
		return
	}

	target := strings.TrimSuffix(s.ExternalURL, "/") + "/dl/" + link.Token
	h.logger.Debug("stremio: play resolved",
		"infohash", infohash, "file", link.Path, "token", link.Token)
	http.Redirect(w, r, target, http.StatusFound)
}

// fetchDeferredTorrent downloads the .torrent for a tracker-less release
// whose fetch was deferred from search (see prowlarr.Result.DownloadURL),
// returning a magnet that carries its trackers plus the raw bytes to add
// directly. Without one the bare magnet finds no peers on a private
// tracker. Best-effort: when no link is known (e.g. after a restart) or
// the fetch fails, the original magnet is returned unchanged.
func (h *Handler) fetchDeferredTorrent(ctx context.Context, s Settings, infohash, magnet string) (string, []byte) {
	u := h.downloadURLs.get(infohash)
	if u == nil {
		return magnet, nil
	}
	hash, name, trackers, raw, err := h.prowlarr(s).FetchTorrent(ctx, string(u))
	if err != nil || hash != infohash {
		h.logger.Warn("stremio: deferred .torrent fetch failed, using bare magnet",
			"infohash", infohash, "fetched", hash, "error", err)
		return magnet, nil
	}
	if _, dn, err := metainfo.FromMagnet(magnet); err == nil && dn != "" {
		name = dn
	}
	h.torrentFiles.put(infohash, raw)
	return prowlarr.SynthesizeMagnet(hash, name, trackers), raw
}

// fetchByKey downloads the .torrent stashed under a deferred play key and
// returns its infohash, a magnet carrying its trackers, and the raw bytes.
// ok is false when the key is unknown (e.g. after a restart or past the
// cache TTL) or the fetch fails.
func (h *Handler) fetchByKey(ctx context.Context, s Settings, key string) (infohash, magnet string, raw []byte, ok bool) {
	u := h.downloadURLs.get(key)
	if u == nil {
		h.logger.Warn("stremio: deferred play key unknown", "key", key)
		return "", "", nil, false
	}
	res, err := h.prowlarr(s).Fetch(ctx, prowlarr.Result{DownloadURL: string(u)})
	if err != nil {
		h.logger.Warn("stremio: deferred .torrent fetch failed", "key", key, "error", err)
		return "", "", nil, false
	}
	h.torrentFiles.put(res.InfoHash, res.TorrentFile)
	return res.InfoHash, res.MagnetURL, res.TorrentFile, true
}
