package app

import "testing"

func TestMatchScore(t *testing.T) {
	name := "The.Apothecary.Diaries.S03E04.1080p.CR.WEB-DL.AAC2.0.H.264-BiOMA.mkv"
	hash := "edb02ad5aa"
	for _, tc := range []struct {
		q  string
		ok bool
	}{
		{"", true},
		{"apothecary", true},
		{"Apothecary Diaries", true},
		{"diaries apothecary", true}, // any word order
		{"apothecary.diaries", true}, // separators ignored
		{"s03e04", true},
		{"apth drs", true}, // letters in order
		{"edb02a", true},   // hash prefix
		{"frieren", false},
		{"s04e03", false},
	} {
		if _, ok := matchScore(tc.q, name, hash); ok != tc.ok {
			t.Errorf("%q: match=%v want %v", tc.q, ok, tc.ok)
		}
	}
	exact, _ := matchScore("apothecary diaries", name, hash)
	words, _ := matchScore("diaries apothecary", name, hash)
	fuzzy, _ := matchScore("apth drs", name, hash)
	if exact <= words || words <= fuzzy {
		t.Errorf("ranking wrong: exact=%d words=%d fuzzy=%d", exact, words, fuzzy)
	}
}
