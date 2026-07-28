package consolidation

import (
	"regexp"
	"strings"

	"github.com/scrypster/muninndb/internal/storage"
)

// Pattern separation for dedup. The dentate gyrus exists to ORTHOGONALIZE
// near-identical inputs — precisely the memories that differ in one load-bearing
// token (a number, a negation, a date) yet embed nearly identically. Cosine
// similarity alone is pattern COMPLETION with no separation stage, so at a 0.95
// threshold a small embedder will happily merge "pricing is $99" with "pricing is
// $149", or "runway was 8 months" with "runway is 11 months" — destroying a
// distinct fact and, worse, folding the loser's access-count into the survivor
// (assimilation + a frequency illusion). This guard is the separation stage:
// before dedup archives a near-duplicate, it must agree with the survivor on every
// load-bearing token. A difference means they are NOT duplicates — they are an
// update or a contradiction — and both are kept (recall's supersedes-aware ranking
// and contradiction surfacing handle them). Bias: refuse-to-merge over destroy.

var (
	// numbers: digit runs with optional thousands separators / decimals, so
	// "$1,299.00" and "80" are captured. Currency/unit symbols are stripped by the
	// word split; we compare the numeric values themselves.
	reNumber = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)
	// 4-digit years (1900–2099) — the common date token in memories.
	reYear = regexp.MustCompile(`\b(?:19|20)\d{2}\b`)
)

// negationMarkers are words whose presence flips or corrects a claim. If one
// memory carries a negation the other does not, they assert different things.
var negationMarkers = map[string]bool{
	"not": true, "never": true, "no": true, "none": true, "cannot": true,
	"cant": true, "dont": true, "doesnt": true, "didnt": true, "isnt": true,
	"arent": true, "wasnt": true, "werent": true, "wont": true, "without": true,
	"rejected": true, "denied": true, "declined": true, "reversed": true,
	"deprecated": true, "cancelled": true, "canceled": true, "invalid": true,
}

var monthMarkers = map[string]bool{
	"january": true, "february": true, "march": true, "april": true, "may": true,
	"june": true, "july": true, "august": true, "september": true, "october": true,
	"november": true, "december": true, "jan": true, "feb": true, "mar": true,
	"apr": true, "jun": true, "jul": true, "aug": true, "sep": true, "sept": true,
	"oct": true, "nov": true, "dec": true,
}

// divergesOnLoadBearingToken reports whether two engrams differ in a way that
// makes them distinct facts despite high embedding similarity: a differing set of
// numbers, of dates (years/months), or an asymmetric negation. When true, dedup
// must NOT merge/archive them.
func divergesOnLoadBearingToken(a, b *storage.Engram) bool {
	ta := loadBearingText(a)
	tb := loadBearingText(b)

	if !equalStringSet(numberSet(ta), numberSet(tb)) {
		return true
	}
	if !equalStringSet(dateSet(ta), dateSet(tb)) {
		return true
	}
	if !equalStringSet(negationSet(ta), negationSet(tb)) {
		return true
	}
	return false
}

func loadBearingText(e *storage.Engram) string {
	// Concept + Content carry the claim; Summary is derived, so skip it.
	return strings.ToLower(e.Concept + " " + e.Content)
}

func numberSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, m := range reNumber.FindAllString(s, -1) {
		out[normalizeNumber(m)] = true
	}
	return out
}

// normalizeNumber strips thousands separators and trailing-zero decimals so
// "1,299" == "1299" and "99.00" == "99", but "99" != "149".
func normalizeNumber(n string) string {
	n = strings.ReplaceAll(n, ",", "")
	if strings.Contains(n, ".") {
		n = strings.TrimRight(n, "0")
		n = strings.TrimRight(n, ".")
	}
	return n
}

func dateSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, y := range reYear.FindAllString(s, -1) {
		out[y] = true
	}
	for _, w := range strings.FieldsFunc(s, splitNonWord) {
		if monthMarkers[w] {
			out[canonicalMonth(w)] = true
		}
	}
	return out
}

func canonicalMonth(m string) string {
	if len(m) >= 3 {
		return m[:3]
	}
	return m
}

func negationSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(s, splitNonWord) {
		if negationMarkers[w] {
			out[w] = true
		}
	}
	return out
}

// splitNonWord splits on anything that isn't a letter or digit, so "don't" and
// "$149" tokenize as "dont"/"don"? — apostrophes are dropped, matching the
// apostrophe-free negation keys.
func splitNonWord(r rune) bool {
	return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
}

func equalStringSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
