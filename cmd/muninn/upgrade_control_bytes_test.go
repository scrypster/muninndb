package main

import "testing"

// A release body reaches the operator's terminal verbatim. An ESC is an escape
// sequence rather than a character, so an unsanitized note could move the
// cursor or overwrite text the operator already read — printing something other
// than what a reviewer saw on the release page.
//
// RED without stripTerminalControlBytes: the ESC and the bell survive into the
// rendered note.
func TestStripMarkdownEmphasis_DropsTerminalControlBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"ansi colour escape", "safe\x1b[31mred", "safe[31mred"},
		{"cursor reposition", "before\x1b[2Kafter", "before[2Kafter"},
		{"bell", "ding\x07dong", "dingdong"},
		{"carriage return overwrite", "real\rfake", "realfake"},
		{"DEL", "a\x7fb", "ab"},
		{"C1 CSI", "a\u009bb", "ab"},
		{"raw invalid byte survives as U+FFFD, which is inert", "a\x9bb", "a\uFFFDb"},
		{"tab and newline survive", "a\tb\nc", "a\tb\nc"},
		{"plain text untouched", "an ordinary note", "an ordinary note"},
		{"markdown still stripped", "**bold** and `code`", "bold and code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripMarkdownEmphasis(tc.in); got != tc.want {
				t.Errorf("stripMarkdownEmphasis(%q) = %q, want %q — a control byte reaching the terminal can rewrite what the operator sees", tc.in, got, tc.want)
			}
		})
	}
}
