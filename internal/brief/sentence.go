package brief

import (
	"strings"
	"unicode"
)

// Split splits text into sentences on period/question/exclamation boundaries.
// Each sentence is trimmed. Empty sentences are dropped.
// Sentences longer than maxLen chars are truncated at the last word boundary.
// If maxLen <= 0, no truncation is applied.
func Split(text string, maxLen int) []string {
	var sentences []string
	var current strings.Builder
	runes := []rune(text)

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		current.WriteRune(r)

		// Check for sentence-ending punctuation followed by space or end of text
		if r == '.' || r == '?' || r == '!' {
			// Lookahead: check if next character is space or end
			isEndOfSentence := false
			if i+1 >= len(runes) {
				isEndOfSentence = true
			} else if i+1 < len(runes) && unicode.IsSpace(runes[i+1]) {
				isEndOfSentence = true
			}

			if isEndOfSentence {
				sentence := strings.TrimSpace(current.String())

				// Skip empty sentences
				if len(sentence) == 0 {
					current.Reset()
					continue
				}

				// Truncate if maxLen is set
				if maxLen > 0 && len(sentence) > maxLen {
					sentence = truncateAtWordBoundary(sentence, maxLen)
				}

				// Only add non-empty sentences after truncation
				if len(sentence) > 0 {
					sentences = append(sentences, sentence)
				}
				current.Reset()

				// Skip following whitespace
				for i+1 < len(runes) && unicode.IsSpace(runes[i+1]) {
					i++
				}
			}
		}
	}

	// Handle any remaining text as the last sentence
	if current.Len() > 0 {
		sentence := strings.TrimSpace(current.String())
		if len(sentence) > 0 {
			if maxLen > 0 && len(sentence) > maxLen {
				sentence = truncateAtWordBoundary(sentence, maxLen)
			}
			if len(sentence) > 0 {
				sentences = append(sentences, sentence)
			}
		}
	}

	return sentences
}

// truncateAtWordBoundary truncates text to maxLen characters,
// breaking at the last space before maxLen rather than mid-word.
func truncateAtWordBoundary(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}

	// Find the last space before or at maxLen
	truncated := text[:maxLen]
	lastSpace := strings.LastIndex(truncated, " ")
	if lastSpace > 0 {
		// Truncate at the last space, but keep the punctuation if present
		truncated = strings.TrimSpace(truncated[:lastSpace])
		// Add back a period if it doesn't already end with punctuation
		if len(truncated) > 0 {
			lastRune := rune(truncated[len(truncated)-1])
			if lastRune != '.' && lastRune != '?' && lastRune != '!' {
				truncated += "."
			}
		}
		return truncated
	}

	// No space found, just truncate and add ellipsis
	return strings.TrimSpace(text[:maxLen]) + "..."
}
