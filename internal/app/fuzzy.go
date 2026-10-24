package app

import (
	"strings"
	"unicode"
)

// normalizeName lower-cases a torrent name and turns separators (. _ - [ ] ( ))
// into single spaces, so "The.Show.S01E02-GRP" reads as "the show s01e02 grp".
func normalizeName(s string) string {
	var b strings.Builder
	space := true
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			space = false
			continue
		}
		if !space {
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

func isHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return s != ""
}

// matchScore reports whether a torrent matches a search query and how well
// (higher is better). It accepts, in order of preference: a hash prefix, the
// whole query as a substring, every word as a substring in any order, and
// finally the query's letters in order with gaps ("apth s3e4").
func matchScore(query, name, hash string) (int, bool) {
	q := normalizeName(query)
	if q == "" {
		return 0, true
	}
	if raw := strings.ToLower(strings.TrimSpace(query)); len(raw) >= 4 && isHex(raw) && strings.HasPrefix(hash, raw) {
		return 4000, true
	}
	n := normalizeName(name)
	if i := strings.Index(n, q); i >= 0 {
		return 3000 - min(i, 999), true
	}
	words := strings.Fields(q)
	all := true
	for _, w := range words {
		if !strings.Contains(n, w) {
			all = false
			break
		}
	}
	if all {
		return 2000, true
	}
	// Subsequence match ignoring spaces; fewer gaps score higher.
	needle := strings.ReplaceAll(q, " ", "")
	hay := strings.ReplaceAll(n, " ", "")
	gaps, j, last := 0, 0, -1
	for i := 0; i < len(hay) && j < len(needle); i++ {
		if hay[i] == needle[j] {
			if last >= 0 && i != last+1 {
				gaps++
			}
			last = i
			j++
		}
	}
	if j < len(needle) {
		return 0, false
	}
	return max(1000-gaps*10, 1), true
}
