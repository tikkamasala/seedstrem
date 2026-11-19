package store

import (
	"context"
	"testing"
)

func TestTorrentsBySeason(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seed := []Torrent{
		{ID: "AAAAAAAAAAAAA", Hash: "aa", Phase: PhaseAdded, AddedAt: 1, ContentSource: "tt", ContentRef: "tt1", Season: 1, Episode: 1},
		{ID: "BBBBBBBBBBBBB", Hash: "bb", Phase: PhaseAdded, AddedAt: 2, ContentSource: "tt", ContentRef: "tt1", Season: 1, Episode: 4},
		{ID: "CCCCCCCCCCCCC", Hash: "cc", Phase: PhaseAdded, AddedAt: 3, ContentSource: "tt", ContentRef: "tt1", Season: 2, Episode: 1},
		{ID: "DDDDDDDDDDDDD", Hash: "dd", Phase: PhaseAdded, AddedAt: 4, ContentSource: "tt", ContentRef: "tt2", Season: 1, Episode: 1},
	}
	for _, tor := range seed {
		if err := s.InsertTorrent(ctx, tor); err != nil {
			t.Fatalf("insert %s: %v", tor.ID, err)
		}
	}

	got, err := s.TorrentsBySeason(ctx, "tt", "tt1", 1)
	if err != nil {
		t.Fatalf("by season: %v", err)
	}
	// Every episode of tt1 season 1, newest-first; other seasons and
	// other series excluded.
	if len(got) != 2 || got[0].ID != "BBBBBBBBBBBBB" || got[1].ID != "AAAAAAAAAAAAA" {
		t.Errorf("got %+v; want [BBBBBBBBBBBBB AAAAAAAAAAAAA]", got)
	}

	blank, err := s.TorrentsBySeason(ctx, "tt", "", 1)
	if err != nil {
		t.Fatalf("blank ref: %v", err)
	}
	if blank != nil {
		t.Errorf("blank ref got %+v; want nil", blank)
	}
}
