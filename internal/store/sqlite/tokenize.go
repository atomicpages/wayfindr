package sqlite

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// minTokenLen is the shortest token kept, in runes (design §7.5).
const minTokenLen = 2

// tokenize splits indexed text into lowercase search tokens. Each word from
// words is followed by its folded form when foldPlural changes it, so
// list_issues becomes [list issues issue]: a singular query matches the
// plural, and the surface form stays available to typo expansion.
func tokenize(text string) []string {
	var tokens []string

	for _, w := range words(text) {
		tokens = append(tokens, w)

		if folded := foldPlural(w); folded != w {
			tokens = append(tokens, folded)
		}
	}

	return tokens
}

// words splits text into lowercase words: on non-alphanumerics, then on
// camelCase and letter/digit boundaries, dropping words shorter than
// minTokenLen. createIssue, create_issue, and create-issue all become
// [create issue]. Case is read before lowercasing, since it marks the
// camelCase boundaries.
func words(text string) []string {
	var out []string
	emit := func(word []rune) {
		if len(word) >= minTokenLen {
			out = append(out, strings.ToLower(string(word)))
		}
	}
	isWordRune := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

	for _, word := range strings.FieldsFunc(text, func(r rune) bool { return !isWordRune(r) }) {
		runes := []rune(word)
		start := 0

		for i := 1; i < len(runes); i++ {
			if isBoundary(runes, i) {
				emit(runes[start:i])
				start = i
			}
		}

		emit(runes[start:])
	}

	return out
}

// minFoldLen is the shortest token, in runes, that foldPlural changes.
const minFoldLen = 4

// minIesFoldLen is the shortest token, in runes, whose ies ending folds to y.
// A 4-rune ies word is a singular verb or noun with a short stem (ties, lies,
// dies), so it is returned unchanged rather than folded to ty or ly.
const minIesFoldLen = 5

// foldPlural reduces an English plural to its singular with a few suffix
// rules (design §7.5), so a query for "invoice" matches list_invoices. It is
// not a stemmer: the rules are applied in order, the first match wins, and
// tokens shorter than minFoldLen are left alone.
//
//	ies            → y      queries → query (minIesFoldLen+ runes; lies stays)
//	sses           → ss     classes → class
//	xes, ches, shes → drop es  indexes → index, branches → branch
//	ss, us, is     → unchanged  class, status, analysis
//	s              → drop s  invoices → invoice
func foldPlural(tok string) string {
	n := runeLen(tok)

	switch {
	case n < minFoldLen:
		return tok
	case strings.HasSuffix(tok, "ies"):
		if n < minIesFoldLen {
			return tok
		}

		return strings.TrimSuffix(tok, "ies") + "y"
	case strings.HasSuffix(tok, "sses"),
		strings.HasSuffix(tok, "xes"),
		strings.HasSuffix(tok, "ches"),
		strings.HasSuffix(tok, "shes"):
		return strings.TrimSuffix(tok, "es")
	case strings.HasSuffix(tok, "ss"),
		strings.HasSuffix(tok, "us"),
		strings.HasSuffix(tok, "is"):
		return tok
	default:
		return strings.TrimSuffix(tok, "s")
	}
}

// isBoundary reports whether a token boundary falls between runes[i-1] and
// runes[i]: a letter/digit change, a lower-to-upper change (fooBar), or the
// last capital of an acronym that starts a new word (HTTPServer → HTTP Server).
func isBoundary(runes []rune, i int) bool {
	prev, cur := runes[i-1], runes[i]

	if unicode.IsDigit(prev) != unicode.IsDigit(cur) {
		return true
	}

	if unicode.IsLower(prev) && unicode.IsUpper(cur) {
		return true
	}

	return unicode.IsUpper(prev) && unicode.IsUpper(cur) &&
		i+1 < len(runes) && unicode.IsLower(runes[i+1])
}

// trigrams returns the distinct trigrams of a token, padded the way pg_trgm
// pads a word (two spaces before, one after) so short tokens and their edges
// still produce useful overlaps.
func trigrams(token string) map[string]struct{} {
	padded := []rune("  " + token + " ")
	set := make(map[string]struct{}, len(padded))

	for i := 0; i+3 <= len(padded); i++ {
		set[string(padded[i:i+3])] = struct{}{}
	}

	return set
}

// jaccard is the trigram Jaccard similarity of two trigram sets.
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}

	inter := 0

	for g := range a {
		if _, ok := b[g]; ok {
			inter++
		}
	}

	return float64(inter) / float64(len(a)+len(b)-inter)
}

// runeLen is the token length the design's thresholds are stated in.
func runeLen(s string) int { return utf8.RuneCountInString(s) }
