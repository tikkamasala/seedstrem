package stremio

import (
	"regexp"
	"strings"

	"github.com/kipsilabs/seedstrem/internal/prowlarr"
)

var (
	// A token that starts the episode/season part of a series release
	// name: S03E15, S03, 3x15, or the word "season"/"saison"/"complete".
	reSeriesMarker = regexp.MustCompile(`^(s\d{1,3}(e\d{1,4})?|\d{1,2}x\d{1,4}|season|saison|complete|integrale)$`)
	// A year token (release names often append the show's first-air year).
	reYearToken = regexp.MustCompile(`^(19|20)\d{2}$`)
	// Leading "[Group]" / "(Group)" tags some releases are prefixed with.
	reLeadingTag = regexp.MustCompile(`^\s*(\[[^\]]*\]|\([^)]*\))\s*`)
)

// countryTokens are region suffixes that distinguish same-named shows
// ("The Office US"); a release carrying one still names the show.
var countryTokens = map[string]bool{"us": true, "uk": true, "au": true, "nz": true, "ca": true}

// titleTokens lowercases s and splits it into alphanumeric words, folding
// "&" into "and" and dropping apostrophes so "Marvel's" == "Marvels".
func titleTokens(s string) []string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "&", " and ")
	s = strings.NewReplacer("'", "", "’", "").Replace(s)
	return strings.FieldsFunc(s, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
}

// matchesSeriesTitle reports whether release names the series title. The
// release's show name is the part before its season/episode marker; it
// must equal the title, optionally followed by a year and/or a country
// code. Text-fallback searches need this: a generic title (e.g. "H")
// makes indexers return releases of every show with a matching S03E15,
// which would otherwise crowd the real one out of the result cap.
func matchesSeriesTitle(release, title string) bool {
	want := titleTokens(title)
	// Cinemeta titles sometimes carry the year: "Tracker (2024)".
	if n := len(want); n > 1 && reYearToken.MatchString(want[n-1]) {
		want = want[:n-1]
	}
	if len(want) == 0 {
		return true // nothing to compare against
	}

	got := titleTokens(reLeadingTag.ReplaceAllString(release, ""))
	marker := len(got)
	for i, tok := range got {
		if reSeriesMarker.MatchString(tok) {
			marker = i
			break
		}
	}
	name := got[:marker]

	if len(name) < len(want) {
		return false
	}
	for i, tok := range want {
		if name[i] != tok {
			return false
		}
	}
	if marker == len(got) {
		// No season/episode marker: only the leading words can be
		// trusted to be the show name, so a prefix match is enough.
		return true
	}
	// Allow a trailing year and/or country code: "Tracker 2024",
	// "The Office US", "Shameless US 2011".
	for _, tok := range name[len(want):] {
		if !reYearToken.MatchString(tok) && !countryTokens[tok] {
			return false
		}
	}
	return true
}

// filterBySeriesTitle keeps only the results whose release name matches
// title (see matchesSeriesTitle).
func filterBySeriesTitle(results []prowlarr.Result, title string) []prowlarr.Result {
	out := results[:0:0]
	for _, r := range results {
		if matchesSeriesTitle(r.Title, title) {
			out = append(out, r)
		}
	}
	return out
}
