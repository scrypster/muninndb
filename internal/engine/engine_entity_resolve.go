package engine

import (
	"context"
	"sort"
	"strings"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// entityResolveMaxCandidates bounds how many ranked fuzzy candidates a
// resolve returns. The best candidate serves retrieval; the rest are
// reported to the caller as near-misses.
const entityResolveMaxCandidates = 5

// entityStopwords are article tokens ignored during fuzzy entity matching —
// they carry no identity: "The Knock" and "knock" name the same entity
// (issue #571).
var entityStopwords = map[string]struct{}{"the": {}, "a": {}, "an": {}}

// entityTokens returns the tokens of an entity name for fuzzy matching:
// NFKC-normalized, lowercased (keys.NormalizeEntityName — the same
// normalization the entity hash index uses), split on separator runes, with
// article stopwords dropped. When a name consists only of stopwords, the raw
// tokens are returned instead so such a name can still match itself.
func entityTokens(name string) []string {
	normalized := keys.NormalizeEntityName(name)
	raw := strings.FieldsFunc(normalized, func(r rune) bool {
		switch r {
		case ' ', '\t', '-', '_', '/', '.':
			return true
		}
		return false
	})
	tokens := make([]string, 0, len(raw))
	for _, tok := range raw {
		if _, stop := entityStopwords[tok]; !stop {
			tokens = append(tokens, tok)
		}
	}
	if len(tokens) == 0 {
		return raw
	}
	return tokens
}

// resolveEntityFuzzy ranks the vault's entity names against the query by
// token overlap. A candidate qualifies when its token set contains, or is
// contained by, the query's token set (one is a subset of the other), or
// when its concatenated token string equals the query's (bridging
// concatenation variants such as "TokenArcade" vs "Token Arcade").
// Containment — not mere overlap — is deliberate: a query sharing only one
// token with an unrelated multi-word entity (e.g. "Token Arcade" vs "Arcade
// Fire") must not qualify just because nothing better exists in the vault.
// Every token-level variant this resolves (dropped/added articles, split or
// joined words) is a subset/superset or exact-token-set relationship, so
// containment loses no real case while ruling out coincidental partial
// overlap. Candidates are ranked by Jaccard overlap of token sets —
// concatenation equality ranks as 1.0 — with a lexicographic tie-break for
// determinism; containment is the only threshold, so there is no similarity
// number to tune.
//
// Vaults hold few distinct entity names, so this is an in-memory scan over
// the 0x20 forward index (ScanVaultEntityNames) — no new index required.
func (e *Engine) resolveEntityFuzzy(ctx context.Context, ws [8]byte, query string) []string {
	qTokens := entityTokens(query)
	if len(qTokens) == 0 {
		return nil
	}
	qSet := make(map[string]struct{}, len(qTokens))
	for _, tok := range qTokens {
		qSet[tok] = struct{}{}
	}
	qJoined := strings.Join(qTokens, "")

	type candidate struct {
		name  string
		score float64
	}
	var cands []candidate
	_ = e.store.ScanVaultEntityNames(ctx, ws, func(name string) error {
		tokens := entityTokens(name)
		if len(tokens) == 0 {
			return nil
		}
		set := make(map[string]struct{}, len(tokens))
		for _, tok := range tokens {
			set[tok] = struct{}{}
		}
		shared := 0
		for tok := range set {
			if _, ok := qSet[tok]; ok {
				shared++
			}
		}
		joinedEqual := strings.Join(tokens, "") == qJoined
		// Containment: one token set must be a subset of the other — shared
		// equals the size of whichever set is smaller.
		contained := shared == len(set) || shared == len(qSet)
		if !contained && !joinedEqual {
			return nil
		}
		score := float64(shared) / float64(len(set)+len(qSet)-shared)
		if joinedEqual {
			score = 1.0
		}
		cands = append(cands, candidate{name: name, score: score})
		return nil
	})

	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].name < cands[j].name
	})
	if len(cands) > entityResolveMaxCandidates {
		cands = cands[:entityResolveMaxCandidates]
	}
	names := make([]string, len(cands))
	for i, c := range cands {
		names[i] = c.name
	}
	return names
}
