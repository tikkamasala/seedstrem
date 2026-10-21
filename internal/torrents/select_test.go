package torrents

import (
	"testing"

	"github.com/kipsilabs/seedstrem/internal/downloader"
)

func TestMatchEpisode(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		season, ep int
		want       bool
	}{
		{"SxxExx", "Show.S01E05.1080p.WEB.mkv", 1, 5, true},
		{"SxxExx wrong ep", "Show.S01E05.1080p.mkv", 1, 6, false},
		{"SxxExx wrong season", "Show.S02E05.1080p.mkv", 1, 5, false},
		{"lowercase s1e5", "show s1e5 720p.mkv", 1, 5, true},
		{"NxM", "Show 1x05 HDTV.avi", 1, 5, true},
		{"NxM wrong season", "Show 2x05 HDTV.avi", 1, 5, false},
		{"episode only E05 season unknown", "[Group] Anime - E05 [1080p].mkv", 0, 5, true},
		{"episode word", "Show Episode 12 finale.mp4", 0, 12, true},
		{"capitulo spanish", "Serie Capitulo 8.mp4", 0, 8, true},
		{"anime dash absolute", "[Grp] Naruto - 05 [720p].mkv", 0, 5, true},
		{"anime dash absolute season1", "[Grp] Naruto - 07 [720p].mkv", 1, 7, true},
		{"bracket absolute", "Anime [12] 1080p.mkv", 0, 12, true},
		{"no match", "Random.Movie.2021.1080p.mkv", 1, 5, false},
		{"zero episode invalid", "Show.S01E00.mkv", 1, 0, false},
		{"padded episode", "Show.S01E005.mkv", 1, 5, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchEpisode(tt.file, tt.season, tt.ep); got != tt.want {
				t.Errorf("matchEpisode(%q, %d, %d) = %v, want %v", tt.file, tt.season, tt.ep, got, tt.want)
			}
		})
	}
}

func f(index int, name string, size int64) downloader.FileInfo {
	return downloader.FileInfo{Index: index, Name: name, Size: size}
}

func TestPickFile(t *testing.T) {
	mb := int64(1 << 20)
	tests := []struct {
		name    string
		files   []downloader.FileInfo
		sel     Selector
		want    int
		wantErr bool
	}{
		{
			name:  "movie largest video",
			files: []downloader.FileInfo{f(0, "readme.txt", 1), f(1, "Movie.mkv", 700*mb), f(2, "extras.mp4", 30*mb)},
			sel:   Selector{IsSeries: false},
			want:  1,
		},
		{
			name:  "movie skips sample",
			files: []downloader.FileInfo{f(0, "Movie.Sample.mkv", 900*mb), f(1, "Movie.mkv", 700*mb)},
			sel:   Selector{IsSeries: false},
			want:  1,
		},
		{
			name:  "series matches episode",
			files: []downloader.FileInfo{f(0, "Show.S01E04.mkv", 500*mb), f(1, "Show.S01E05.mkv", 480*mb)},
			sel:   Selector{IsSeries: true, Season: 1, Episode: 5},
			want:  1,
		},
		{
			name:    "series no match errors",
			files:   []downloader.FileInfo{f(0, "Show.S01E04.mkv", 500*mb)},
			sel:     Selector{IsSeries: true, Season: 1, Episode: 9},
			wantErr: true,
		},
		{
			name:    "no video files errors",
			files:   []downloader.FileInfo{f(0, "notes.txt", 10), f(1, "cover.jpg", 20)},
			sel:     Selector{IsSeries: false},
			wantErr: true,
		},
		{
			name: "scene rar set picks first volume",
			files: []downloader.FileInfo{
				f(0, "Movie.2020-GRP/movie.2020-grp.nfo", 1), f(1, "Movie.2020-GRP/movie.2020-grp.r00", 50*mb),
				f(2, "Movie.2020-GRP/movie.2020-grp.r01", 20*mb), f(3, "Movie.2020-GRP/movie.2020-grp.rar", 50*mb),
				f(4, "Movie.2020-GRP/movie.2020-grp.sfv", 1),
			},
			sel:  Selector{IsSeries: false},
			want: 3,
		},
		{
			name: "rar set skips sample and subs sets",
			files: []downloader.FileInfo{
				f(0, "Rel/Sample/rel-sample.rar", 900*mb),
				f(1, "Rel/Subs/rel.subs.rar", 1*mb),
				f(2, "Rel/rel.r00", 50*mb), f(3, "Rel/rel.rar", 50*mb),
			},
			sel:  Selector{IsSeries: false},
			want: 3,
		},
		{
			name: "video beats rar set",
			files: []downloader.FileInfo{
				f(0, "Rel/rel.r00", 900*mb), f(1, "Rel/rel.rar", 900*mb), f(2, "Rel/rel.mkv", 10*mb),
			},
			sel:  Selector{IsSeries: false},
			want: 2,
		},
		{
			name: "rar season pack matches episode",
			files: []downloader.FileInfo{
				f(0, "Show.S01.720p-GRP/Show.S01E04.720p-GRP/show.s01e04.720p-grp.r00", 50*mb),
				f(1, "Show.S01.720p-GRP/Show.S01E04.720p-GRP/show.s01e04.720p-grp.rar", 50*mb),
				f(2, "Show.S01.720p-GRP/Show.S01E05.720p-GRP/show.s01e05.720p-grp.part1.rar", 50*mb),
				f(3, "Show.S01.720p-GRP/Show.S01E05.720p-GRP/show.s01e05.720p-grp.part2.rar", 10*mb),
			},
			sel:  Selector{IsSeries: true, Season: 1, Episode: 5},
			want: 2,
		},
		{
			name:    "rar set with missing volume errors",
			files:   []downloader.FileInfo{f(0, "Rel/rel.r01", 50*mb), f(1, "Rel/rel.rar", 50*mb)},
			sel:     Selector{IsSeries: false},
			wantErr: true,
		},
		{
			name:  "season pack picks right episode largest",
			files: []downloader.FileInfo{f(0, "S01E05.mkv", 400*mb), f(1, "S01E05.PROPER.mkv", 450*mb), f(2, "S01E06.mkv", 800*mb)},
			sel:   Selector{IsSeries: true, Season: 1, Episode: 5},
			want:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PickFile(tt.files, tt.sel)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got index %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("PickFile = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestIsNextEpisodeAcceptsRarVolumes(t *testing.T) {
	sel := Selector{IsSeries: true, Season: 1, Episode: 4}
	for name, want := range map[string]bool{
		"Show.S01E05-GRP/show.s01e05-grp.rar":        true,
		"Show.S01E05-GRP/show.s01e05-grp.r07":        true,
		"Show.S01E05-GRP/show.s01e05-grp.nfo":        false,
		"Show.S01E05-GRP/Sample/show.s01e05-grp.rar": false,
		"Show.S01E07-GRP/show.s01e07-grp.rar":        false,
	} {
		if got := isNextEpisode(name, sel, 2); got != want {
			t.Errorf("isNextEpisode(%q) = %v, want %v", name, got, want)
		}
	}
}
