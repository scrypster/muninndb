package mcp

import (
	"strings"

	"github.com/scrypster/muninndb/internal/engine/activation"
)

// Relevance-band text: the ONE place that says what a band means to a reader
// (COG-30 (d)). The muninn_recall tool description and the guide both render
// from these, so the two cannot drift apart (principle #6). openapi.yaml
// cannot import a Go constant; TestRelevanceBandWording_NoAnswerClaim pins it.
//
// The band is arithmetic on evidence (activation.RelevanceBand). These
// sentences claim only that: what the predicate computes, plus ordinal
// observations that held on every vault measured. No percentages — numbers
// measured on particular vaults belong in the decision record, not in text
// every vault's agents read (principle #11).

// bandGoverningSentence precedes every full list of band claims.
// The noise floor is the EMBEDDING MODEL's (a per-model lookup, operator-
// overridable via semantic_floor), and the band edges are fixed ratios of the
// default gate and the content ceiling: nothing here is measured per vault.
const bandGoverningSentence = "The band reports how much evidence a memory has against your query, measured against " +
	"the noise floor of the embedding model in use (operator-overridable), with fixed band edges. It does not report whether " +
	"the memory answers your question; nothing in MuninnDB computes that."

const (
	// Not "in wording": a semantic-only row with cosine >= 0.92 bands strong
	// with no word overlap.
	bandClaimStrong = "a close match — the band most likely to answer you directly; still check it is about your question"
	// The design's draft opened this with "related to your query"; its own
	// rename analysis rules "related" out as false (a large share of moderate
	// rows were unrelated), so the claim names evidence, not relatedness.
	// The score is 0.6 semantic + 0.4 lexical on default weights; rows
	// answering another product's questions were measured to score almost
	// entirely on the semantic part — NOT on shared wording.
	bandClaimModerate = "evidence above the noise floor — a candidate to verify, not an established answer: often on topic " +
		"without answering. A moderate score can come almost entirely from semantic similarity to the vault's general subject " +
		"matter, with little or none of your query's own wording matched; that is why a question this vault holds nothing " +
		"about often still returns moderate rows"
	// Hedged: COG-30's cold-start residual says real matches can under-band.
	bandClaimWeak = "within one doubling of the noise floor — hard to tell from noise and usually not the answer, though " +
		"a real match can band this low (for example while its embedding is still pending)"
	bandClaimFilterMatch = "returned because a tag filter named it, not because it matched your text"
	bandClaimUncal       = "no calibrated scale for this row — `relevance_band_basis` says why"
)

// bandClaims is the ordered canonical list, keyed by the wire values.
var bandClaims = []struct{ band, claim string }{
	{activation.RelevanceStrong, bandClaimStrong},
	{activation.RelevanceModerate, bandClaimModerate},
	{activation.RelevanceWeak, bandClaimWeak},
	{activation.RelevanceFilterMatch, bandClaimFilterMatch},
	{activation.RelevanceUncalibrated, bandClaimUncal},
}

// recallBandSentence is the muninn_recall tool description's band text. It
// carries short forms of all five claims (the moderate one keeps its key
// phrase and the reason unanswerable questions land there) —
// to keep the always-loaded description small (every client pays for it on
// every session). The guide carries all five in full. Built from string
// constants so the tool literal stays a compile-time constant.
const recallBandSentence = "The band reports evidence, not whether a memory answers you: " +
	"`strong` = a close match, most likely to answer; " +
	"`moderate` = a candidate to verify, not an established answer: often on topic without answering; its score can be " +
	"almost all semantic similarity to the vault's subject matter, so unanswerable questions often get moderate rows; " +
	"`weak` = near the noise floor; `filter_match` = a tag filter named it; " +
	"`uncalibrated` = see `relevance_band_basis`. " +
	"If none is `strong`, confirm one answers you before relying on it."

// guideBandList renders the governing sentence and the five canonical claims
// as the guide's relevance_band bullet body.
func guideBandList() string {
	var b strings.Builder
	b.WriteString(bandGoverningSentence)
	for _, c := range bandClaims {
		b.WriteString("\n  - `")
		b.WriteString(c.band)
		b.WriteString("` — ")
		b.WriteString(c.claim)
		b.WriteString(".")
	}
	return b.String()
}
