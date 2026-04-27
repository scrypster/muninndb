package main

import (
	"strings"
	"unicode"
)

// tokenize lowercases, strips punctuation, and splits on whitespace.
func tokenize(text string) []string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsPunct(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, text)
	tokens := strings.Fields(cleaned)
	if len(tokens) == 0 {
		return nil
	}
	return tokens
}

// TokenF1 computes token-level F1 between predicted and reference strings.
func TokenF1(predicted, reference string) float64 {
	predTokens := tokenize(predicted)
	refTokens := tokenize(reference)
	if len(predTokens) == 0 && len(refTokens) == 0 {
		return 1.0
	}
	if len(predTokens) == 0 || len(refTokens) == 0 {
		return 0.0
	}

	refSet := make(map[string]int)
	for _, t := range refTokens {
		refSet[t]++
	}

	common := 0
	for _, t := range predTokens {
		if refSet[t] > 0 {
			common++
			refSet[t]--
		}
	}

	if common == 0 {
		return 0.0
	}

	precision := float64(common) / float64(len(predTokens))
	recall := float64(common) / float64(len(refTokens))
	return 2 * precision * recall / (precision + recall)
}

// TokenF1MultiHop splits the reference by commas, computes F1 per sub-answer, returns max.
func TokenF1MultiHop(predicted, referenceCSV string) float64 {
	parts := strings.Split(referenceCSV, ",")
	best := 0.0
	for _, part := range parts {
		f1 := TokenF1(predicted, strings.TrimSpace(part))
		if f1 > best {
			best = f1
		}
	}
	return best
}

// AnswerRecall computes what fraction of reference tokens appear in the predicted text.
// Better suited for retrieval-only evaluation where predicted text is much longer than reference.
func AnswerRecall(predicted, reference string) float64 {
	predTokens := tokenize(predicted)
	refTokens := tokenize(reference)
	if len(refTokens) == 0 {
		return 1.0
	}
	if len(predTokens) == 0 {
		return 0.0
	}

	predSet := make(map[string]int)
	for _, t := range predTokens {
		predSet[t]++
	}

	found := 0
	for _, t := range refTokens {
		if predSet[t] > 0 {
			found++
			predSet[t]--
		}
	}
	return float64(found) / float64(len(refTokens))
}

// AnswerRecallMultiHop splits reference by commas and returns max recall.
func AnswerRecallMultiHop(predicted, referenceCSV string) float64 {
	parts := strings.Split(referenceCSV, ",")
	best := 0.0
	for _, part := range parts {
		r := AnswerRecall(predicted, strings.TrimSpace(part))
		if r > best {
			best = r
		}
	}
	return best
}

// AdversarialScore returns 1.0 if the response indicates no information, else 0.0.
func AdversarialScore(response string) float64 {
	lower := strings.ToLower(response)
	markers := []string{
		"no information available",
		"not mentioned",
		"no information",
		"cannot be determined",
		"not available",
		"no relevant",
	}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return 1.0
		}
	}
	return 0.0
}
