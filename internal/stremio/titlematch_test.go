package stremio

import "testing"

// TestMatchesSeriesTitle covers the text-fallback relevance filter. Add a
// row per real-world title/release pair worth pinning down.
func TestMatchesSeriesTitle(t *testing.T) {
	tests := []struct {
		title   string // Cinemeta series name
		release string // indexer release title
		want    bool
	}{
		// Single-letter title: the case that motivated the filter.
		{"H", "H.S03E15.FRENCH.1080p.WEB.H264-UKDTV", true},
		{"H", "H.S03E15.FRENCH.720p.WEB.H264-UKDTV", true},
		{"H", "H S03 FRENCH 1080p WEB H264-UKDTV", true},
		{"H", "Mountain.Men.S03E15.FRENCH.1080p.WEB.x264-HEADER", false},
		{"H", "Fire.Force.S03E15.MULTi.1080p.WEBRiP.x265-T3KASHi", false},
		{"H", "Tracker 2024 S03E15 1080p WEB h264-ETHEL", false},
		{"H", "The Ark 2023 S03 1080p 10bit WEBRip 6CH x265 HEVC-PSA", false},

		// Punctuation, case and separators don't matter.
		{"The Office", "the.office.s02e01.720p", true},
		{"Marvel's Agents of S.H.I.E.L.D.", "Marvels.Agents.of.S.H.I.E.L.D.S01E01.1080p", true},
		{"Law & Order", "Law.and.Order.S03E15.MULTi.1080p.BluRay.x264-UKDTV", true},

		// Trailing year / country code after the show name are allowed.
		{"Tracker", "Tracker 2024 S03E15 1080p WEB h264-ETHEL", true},
		{"Tracker (2024)", "Tracker.S01E01.1080p", true},
		{"The Office", "The.Office.US.S05E01.1080p", true},
		{"Shameless", "Shameless.US.2011.S01E01.720p", true},

		// A longer show sharing the title as a prefix is a different show.
		{"The Office", "The.Office.Christmas.Special.S01E01", false},
		{"Law & Order", "Law.and.Order.Special.Victims.Unit.S03E15.1080p", false},

		// Season markers other than SxxEyy.
		{"The Show", "The.Show.1x05.720p", true},
		{"The Show", "The Show Season 1 COMPLETE 720p", true},
		{"The Show", "The.Show.Complete.Series.1080p", true},

		// Leading group tags are ignored.
		{"The Show", "[Group] The Show S01E01 1080p", true},
		{"The Show", "[Group] Other Show S01E01 1080p", false},

		// No marker: a prefix match is accepted.
		{"The Show", "The.Show.Pilot.1080p", true},
		{"The Show", "Other.Show.1080p", false},
	}
	for _, tt := range tests {
		if got := matchesSeriesTitle(tt.release, tt.title); got != tt.want {
			t.Errorf("matchesSeriesTitle(%q, %q) = %v, want %v", tt.release, tt.title, got, tt.want)
		}
	}
}
