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
// it derives a title from text's leading part (leading whitespace trimmed
// first, so a decision that opens with blank lines never yields an empty or
// ellipsis-only concept). The result is never longer than max bytes and is
// always valid UTF-8. Cut preference, in order: the last sentence end at or
// past a third of the budget; the last word boundary anywhere inside the cap;
// only for a text with no boundary at all, a hard cut — backed off so it never
// lands inside a combining-mark or zero-width-joiner sequence (a cheap
// grapheme approximation, not full UAX #29). A cut that does not end on a
// sentence gets a trailing ellipsis. cut reports whether the result differs
// from text. Content is the caller's job: this only ever shapes a title.
func conceptFromText(text string, max int) (concept string, cut bool) {
	if len(text) <= max {
		return text, false
	}
	body := strings.TrimLeftFunc(text, unicode.IsSpace)
	if len(body) <= max {
		return strings.TrimRightFunc(body, unicode.IsSpace), true
	}
	const ellipsis = "…"
	budget := max - len(ellipsis)
	end := budget
	for end > 0 && !utf8.RuneStart(body[end]) {
		end--
	}
	head := body[:end]
	floor := budget / 3

	// Sentence end: terminal punctuation followed by whitespace (or a newline).
	for i := len(head) - 1; i >= floor && i > 0; i-- {
		c := head[i]
		if c == '\n' || ((c == ' ' || c == '\t') && strings.ContainsRune(".!?", rune(head[i-1]))) {
			if out := strings.TrimRightFunc(head[:i], unicode.IsSpace); out != "" {
				return out, true
			}
		}
	}
	// Word boundary: the last whitespace inside the cap, wherever it is.
	if i := strings.LastIndexFunc(head, unicode.IsSpace); i > 0 {
		out := strings.TrimRightFunc(strings.TrimRight(strings.TrimRightFunc(head[:i], unicode.IsSpace), ",;:-–—"), unicode.IsSpace)
		if out != "" {
			return out + ellipsis, true
		}
	}
	// No boundary: hard cut, but not inside a grapheme-ish cluster — never
	// before a combining mark / joiner / variation selector, never after a
	// joiner.
	for end > 0 {
		next, _ := utf8.DecodeRuneInString(body[end:])
		prev, _ := utf8.DecodeLastRuneInString(body[:end])
		if !joinsPrevious(next) && prev != '\u200D' {
			break
		}
		_, size := utf8.DecodeLastRuneInString(body[:end])
		end -= size
	}
	if end == 0 { // one unbroken cluster longer than the cap: rune-safe cut
		end = len(head)
	}
	return body[:end] + ellipsis, true
}

// joinsPrevious reports whether r attaches to the rune before it (so a cut
// must not separate them): combining marks, ZWJ, variation selectors, emoji
// skin-tone modifiers.
func joinsPrevious(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Mc) ||
		r == '\u200D' || (r >= 0xFE00 && r <= 0xFE0F) || (r >= 0x1F3FB && r <= 0x1F3FF)
}
