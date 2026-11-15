package engine

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/scrypster/muninndb/internal/storage/erf"
)

// maxConceptBytes is the ERF concept cap, re-exported for engine call sites.
const maxConceptBytes = erf.MaxConceptBytes

// conceptFromText returns text unchanged when it fits in max bytes. Otherwise
// it returns a title cut from text's leading part, never longer than max
// bytes and never splitting a UTF-8 rune, preferring (in order) the last
// sentence end and the last word boundary that keep at least a third of the
// budget; only a text with neither is hard-cut. A cut that does not end on a
// sentence is marked with a trailing ellipsis. cut reports whether anything
// was removed. Content is the caller's job: this only ever shapes a title.
func conceptFromText(text string, max int) (concept string, cut bool) {
	if len(text) <= max {
		return text, false
	}
	const ellipsis = "…"
	budget := max - len(ellipsis)
	// Back off to a rune boundary.
	end := budget
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	head := text[:end]
	floor := budget / 3

	// Sentence end: terminal punctuation followed by whitespace (or the
	// newline itself), at or past the floor.
	best := -1
	for i := len(head) - 1; i >= floor && i > 0; i-- {
		c := head[i]
		if c == '\n' {
			best = i
			break
		}
		if (c == ' ' || c == '\t') && strings.ContainsRune(".!?", rune(head[i-1])) {
			best = i
			break
		}
	}
	if best > 0 {
		return strings.TrimRightFunc(head[:best], unicode.IsSpace), true
	}
	// Word boundary.
	if i := strings.LastIndexFunc(head, unicode.IsSpace); i >= floor {
		return strings.TrimRightFunc(strings.TrimRight(strings.TrimRightFunc(head[:i], unicode.IsSpace), ",;:-–—"), unicode.IsSpace) + ellipsis, true
	}
	return head + ellipsis, true
}
