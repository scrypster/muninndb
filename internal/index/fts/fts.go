package fts

import (
	"context"
	"encoding/binary"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/cockroachdb/pebble"
	"github.com/kljensen/snowball"
	"github.com/scrypster/muninndb/internal/metrics"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

const (
	k1 = 1.2
	b  = 0.75

	FieldConcept   = uint8(0x01)
	FieldTags      = uint8(0x02)
	FieldContent   = uint8(0x03)
	FieldCreatedBy = uint8(0x04)

	fieldWeightConcept   = 3.0
	fieldWeightTags      = 2.0
	fieldWeightContent   = 1.0
	fieldWeightCreatedBy = 0.5

	ContentCompressThreshold = 512
)

// stop words — common English words that add no search value
var stopWords = map[string]bool{
	"the": true, "is": true, "a": true, "an": true, "and": true, "or": true,
	"but": true, "in": true, "on": true, "at": true, "to": true, "for": true,
	"of": true, "with": true, "by": true, "from": true, "up": true, "about": true,
	"into": true, "through": true, "this": true, "that": true, "these": true,
	"those": true, "it": true, "its": true, "be": true, "was": true, "were": true,
	"are": true, "been": true, "have": true, "has": true, "had": true, "do": true,
	"does": true, "did": true, "will": true, "would": true, "could": true, "should": true,
	"may": true, "might": true, "can": true, "as": true, "if": true, "then": true,
}

// ScoredID is a scored search result.
type ScoredID struct {
	ID    [16]byte
	Score float64
}

// PostingValue is the 7-byte per-posting entry value.
type PostingValue struct {
	TF     float32
	Field  uint8
	DocLen uint16
}

// idfKey is the composite cache key for the IDF value of a term within a vault.
// Keying by (ws, term) prevents cross-vault IDF contamination.
type idfKey struct {
	ws   [8]byte
	term string
}

// Index is the FTS inverted index backed by Pebble.
type Index struct {
	db *pebble.DB
	mu sync.RWMutex
	// In-memory IDF cache: (vault, term) → idf
	idfCache map[idfKey]float64
	// versionCache caches the FTS schema version per vault (0=legacy dual-path, 1=stemmed-only).
	// Populated lazily on first Search() for each vault; FTSVersionKey is write-once.
	versionCache sync.Map // key: [8]byte wsPrefix, value: byte
}

func New(db *pebble.DB) *Index {
	return &Index{
		db:       db,
		idfCache: make(map[idfKey]float64, 1024),
	}
}

// InvalidateIDFCache clears the in-memory IDF cache, forcing fresh recalculation
// on the next search. Call this after a vault clear to prevent stale IDF values
// from influencing BM25 scoring.
func (idx *Index) InvalidateIDFCache() {
	idx.mu.Lock()
	idx.idfCache = make(map[idfKey]float64)
	idx.mu.Unlock()
}

// tokenizeRaw applies lowercase, character normalization, length filtering,
// and stopword removal — but NOT stemming. Used for backward-compatible
// dual-path search against un-migrated (pre-stemming) indexes.
func tokenizeRaw(text string) []string {
	text = strings.ToLower(text)
	var b strings.Builder
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	tokens := strings.Fields(b.String())
	result := tokens[:0]
	for _, t := range tokens {
		if len(t) < 2 {
			continue
		}
		if stopWords[t] {
			continue
		}
		if len([]rune(t)) > 64 {
			t = string([]rune(t)[:64])
		}
		result = append(result, t)
	}
	return result
}

// Tokenize applies tokenizeRaw then Porter2 stemming.
// New engrams are indexed with stemmed tokens via this function.
func Tokenize(text string) []string {
	raw := tokenizeRaw(text)
	out := make([]string, 0, len(raw))
	for _, tok := range raw {
		stemmed, err := snowball.Stem(tok, "english", true)
		if err == nil && stemmed != "" {
			out = append(out, stemmed)
		} else {
			out = append(out, tok) // fallback: keep original
		}
	}
	return out
}

// Trigrams extracts 3-character windows from a term.
func Trigrams(term string) [][3]byte {
	if len(term) < 3 {
		return nil
	}
	var result [][3]byte
	for i := 0; i+2 < len(term); i++ {
		result = append(result, [3]byte{term[i], term[i+1], term[i+2]})
	}
	return result
}

// encodePosting encodes a PostingValue into 7 bytes.
func encodePosting(pv PostingValue) []byte {
	buf := make([]byte, 7)
	binary.BigEndian.PutUint32(buf[0:4], math.Float32bits(pv.TF))
	buf[4] = pv.Field
	binary.BigEndian.PutUint16(buf[5:7], pv.DocLen)
	return buf
}

// decodePosting decodes 7 bytes into a PostingValue.
func decodePosting(buf []byte) PostingValue {
	if len(buf) < 7 {
		return PostingValue{}
	}
	return PostingValue{
		TF:     math.Float32frombits(binary.BigEndian.Uint32(buf[0:4])),
		Field:  buf[4],
		DocLen: binary.BigEndian.Uint16(buf[5:7]),
	}
}

// fieldWeight returns the scoring weight for a field.
func fieldWeight(field uint8) float64 {
	switch field {
	case FieldConcept:
		return fieldWeightConcept
	case FieldTags:
		return fieldWeightTags
	case FieldContent:
		return fieldWeightContent
	case FieldCreatedBy:
		return fieldWeightCreatedBy
	default:
		return 1.0
	}
}

// Document is the indexable text of one engram — the fields whose Tokenize
// output becomes posting-list terms. Used by ReindexEngram to name the before
// and after states of a single engram without a ten-argument signature.
type Document struct {
	Concept   string
	CreatedBy string
	Content   string
	Tags      []string
}

// docTermCounts returns the per-(term, field) term frequency for a document plus
// its total token length — the BM25 length normalizer, which must count every
// indexed field. Shared by IndexEngram and ReindexEngram so the two entry points
// cannot drift on what a document's terms are.
func docTermCounts(concept, createdBy, content string, tags []string) (map[string]map[uint8]int, uint16) {
	termCounts := make(map[string]map[uint8]int)
	addTerms := func(text string, field uint8) {
		tokens := Tokenize(text)
		for _, t := range tokens {
			if termCounts[t] == nil {
				termCounts[t] = make(map[uint8]int)
			}
			termCounts[t][field]++
		}
	}

	addTerms(concept, FieldConcept)
	addTerms(createdBy, FieldCreatedBy)
	addTerms(content, FieldContent)
	for _, tag := range tags {
		addTerms(tag, FieldTags)
	}

	allTokens := Tokenize(concept + " " + content + " " + createdBy + " " + strings.Join(tags, " "))
	return termCounts, uint16(len(allTokens))
}

// docTermSet returns the set of distinct terms a document was indexed under.
// This is document-level membership, deliberately NOT per-field: a tag token can
// coincide with a content token, and the two entry points that reason about a
// term entering or leaving the index (DeleteEngram, ReindexEngram) must agree on
// what "the document contains this term" means.
func docTermSet(concept, createdBy, content string, tags []string) map[string]struct{} {
	termSet := make(map[string]struct{})
	addTerms := func(text string) {
		for _, t := range Tokenize(text) {
			termSet[t] = struct{}{}
		}
	}

	addTerms(concept)
	addTerms(createdBy)
	addTerms(content)
	for _, tag := range tags {
		addTerms(tag)
	}
	return termSet
}

// IndexEngram writes FTS posting list entries for an engram.
// ws is the 8-byte workspace prefix. id is the ULID.
func (idx *Index) IndexEngram(ws [8]byte, id [16]byte, concept, createdBy, content string, tags []string) error {
	// Collect all (term, field, docLen) tuples
	termCounts, docLen := docTermCounts(concept, createdBy, content, tags)

	// Acquire lock BEFORE reading current DF values to prevent lost-update races
	// under concurrent IndexEngram calls.
	idx.mu.Lock()

	// Build a single batch containing both posting lists AND DF updates so that
	// the two writes are committed atomically. A crash after the old two-phase
	// approach (posting batch committed first, DF written separately) could leave
	// posting lists with stale DF counts.
	batch := idx.db.NewBatch()

	for term, fieldCounts := range termCounts {
		for field, count := range fieldCounts {
			pv := PostingValue{
				TF:     float32(count),
				Field:  field,
				DocLen: docLen,
			}
			key := keys.FTSPostingKey(ws, term, field, id)
			val := encodePosting(pv)
			batch.Set(key, val, nil)
		}

		// Write trigrams
		for _, tri := range Trigrams(term) {
			tkey := keys.TrigramKey(ws, tri, id)
			batch.Set(tkey, nil, nil)
		}

		// Read current DF and write updated DF into the same batch.
		tkey := keys.TermStatsKey(ws, term)
		var currentDF uint32
		val, closer, err := idx.db.Get(tkey)
		if err == nil && len(val) >= 4 {
			currentDF = binary.BigEndian.Uint32(val[0:4])
			closer.Close()
		}
		newDF := currentDF + 1
		var dfBuf [8]byte
		binary.BigEndian.PutUint32(dfBuf[:4], newDF)
		batch.Set(tkey, dfBuf[:], nil)

		// Invalidate IDF cache for this term so it's recalculated on next search.
		delete(idx.idfCache, idfKey{ws, term})
	}

	// Commit single atomic batch: posting lists + DF updates land together.
	err := batch.Commit(pebble.Sync)
	idx.mu.Unlock()

	if err != nil {
		return err
	}

	// Update global stats (TotalEngrams, AvgDocLen)
	return idx.UpdateStats(ws, int(docLen))
}

// DeleteEngram removes FTS posting-list and trigram entries for an engram.
// Called from SoftDelete to prevent soft-deleted engrams from appearing in search results.
// Does NOT update global stats (stats are approximate; no need to recount on soft delete).
func (idx *Index) DeleteEngram(ws [8]byte, id [16]byte, concept, createdBy, content string, tags []string) error {
	// Collect all unique terms that were indexed for this engram.
	termSet := docTermSet(concept, createdBy, content, tags)

	if len(termSet) == 0 {
		return nil
	}

	idx.mu.Lock()
	batch := idx.db.NewBatch()

	for term := range termSet {
		// Delete posting-list keys for this engram across all fields.
		// Deleting a non-existent key is a no-op in Pebble.
		for _, field := range []uint8{FieldConcept, FieldContent, FieldTags, FieldCreatedBy} {
			key := keys.FTSPostingKey(ws, term, field, id)
			batch.Delete(key, nil)
		}

		// Delete trigram keys for this term.
		for _, tri := range Trigrams(term) {
			tkey := keys.TrigramKey(ws, tri, id)
			batch.Delete(tkey, nil)
		}

		// Invalidate IDF cache for this term — DF is now stale.
		delete(idx.idfCache, idfKey{ws, term})
	}

	err := batch.Commit(pebble.Sync)
	idx.mu.Unlock()
	return err
}

// ReindexEngram atomically replaces every FTS entry for ONE engram: it drops the
// postings and trigrams derived from prev, then writes the ones derived from
// next, under a single idx.mu and a single Pebble batch. It is the entry point
// for an in-place edit of an already-indexed engram (Engine.UpdateTags); it does
// not change IndexEngram or DeleteEngram, which keep serving first-write and
// delete.
//
// The reason it exists rather than callers pairing DeleteEngram with IndexEngram
// is that the pair is NOT statistics-neutral, and the corpus statistics are what
// BM25 — and therefore COG-24's full_text_relevance — is defined over:
//
//   - IndexEngram calls UpdateStats (TotalEngrams/AvgDocLen) and DeleteEngram
//     deliberately does not, so the pair inflates the corpus size N by one per
//     call. Second-order: idfMax grows like ln N.
//   - IndexEngram increments the per-term document frequency df_t for EVERY term
//     of the document, and DeleteEngram decrements none. That is first-order and
//     per-call. getIDF is log((N−df+0.5)/(df+0.5)+1) ≡ ln((N+1)/(df+0.5)), so a
//     retag's (N, df) → (N+1, df+1) barely moves the numerator and moves the
//     denominator by a whole document: the engram's IDF — and with it its own
//     score for a query on its own unchanged content — decays on every edit.
//     Measured on a 60-engram corpus with a rare target term: one retag cost the
//     target 8.1% of its score, ten took it below a 0.3 threshold, and 100 cost
//     82.6% against a 10.0% drop for an unretagged control in the same vault.
//     A memory that becomes unfindable BECAUSE its owner curates it is the
//     silently-wrong failure class, and nothing surfaces it (#720).
//
// So: no UpdateStats call, and df_t moves only for terms whose document-level
// membership actually changed — +1 for a term that entered, −1 for one that
// left, untouched for a term present in both. Membership is per TERM over the
// whole document, not per field, because a tag token can coincide with a content
// token: "due" leaving the tag set is not a DF change if "due" is still in the
// content.
//
// Deletes are queued before adds so a retained term ends up holding the ADD's
// posting — Pebble applies a batch in key-op order, and the same holds for a
// trigram shared between a departing term and a surviving one.
//
// What "no UpdateStats" costs, stated beside the benefit so a future reader does
// not take the stats-neutrality above as immunity. This method writes every
// posting at the NEW docLen but leaves AvgDocLen alone, so an in-place edit that
// changes the tag set's token count is length-penalized against a stale corpus
// average. Measured on one 60-engram corpus, one target, one query:
//
//	[constant   ] retags 0/1/10/50 → 0.1477 throughout    (avgdl 7.100, flat)
//	[growing    ] retags 0→50, tags 1→51 → 0.1690 → 0.0486
//	[oscillating] retags 0/1/10/50 → 0.1477 / 0.1216 / 0.1477 / 0.1477
//
// So the neutrality is with respect to retag COUNT, not to tag CONTENT. The
// oscillating row is the proof: the score returns EXACTLY to 0.1477 whenever the
// tag set returns, with no hysteresis and no per-call ratchet, because a posting's
// value is a pure function of the current document and df_t moves only on
// membership. That is ordinary BM25 length normalization, not drift — which is the
// whole difference from the DeleteEngram+IndexEngram pair, where the loss
// accumulated per call and never came back.
//
// The residual: AvgDocLen does not track in-place length changes, so a vault
// whose engrams grow their tag sets in place has a corpus average that lags low
// and over-penalizes long documents. Direction is conservative — matches are
// under-credited, never over-credited — and `muninn reindex-fts` recomputes
// AvgDocLen from scratch. Calling UpdateStats here is NOT the fix: it takes a
// docLen and increments TotalEngrams, which is the N inflation this method exists
// to avoid; a correct AvgDocLen maintenance would need a delta-aware stats update
// (deferred, #720).
func (idx *Index) ReindexEngram(ws [8]byte, id [16]byte, prev, next Document) error {
	oldTerms := docTermSet(prev.Concept, prev.CreatedBy, prev.Content, prev.Tags)
	newCounts, docLen := docTermCounts(next.Concept, next.CreatedBy, next.Content, next.Tags)

	if len(oldTerms) == 0 && len(newCounts) == 0 {
		return nil
	}

	// Lock BEFORE the DF reads in adjustDF, as IndexEngram does: the read-modify-
	// write on the term-stats key is a lost-update race otherwise.
	idx.mu.Lock()
	batch := idx.db.NewBatch()
	defer batch.Close()

	// Drop everything the OLD document put in the index first.
	for term := range oldTerms {
		for _, field := range []uint8{FieldConcept, FieldContent, FieldTags, FieldCreatedBy} {
			batch.Delete(keys.FTSPostingKey(ws, term, field, id), nil)
		}
		for _, tri := range Trigrams(term) {
			batch.Delete(keys.TrigramKey(ws, tri, id), nil)
		}
		delete(idx.idfCache, idfKey{ws, term})
	}

	// ...then write what the NEW document needs, at the new docLen.
	for term, fieldCounts := range newCounts {
		for field, count := range fieldCounts {
			pv := PostingValue{
				TF:     float32(count),
				Field:  field,
				DocLen: docLen,
			}
			batch.Set(keys.FTSPostingKey(ws, term, field, id), encodePosting(pv), nil)
		}
		for _, tri := range Trigrams(term) {
			batch.Set(keys.TrigramKey(ws, tri, id), nil, nil)
		}
		delete(idx.idfCache, idfKey{ws, term})
	}

	// DF adjustments: membership changes only.
	for term := range newCounts {
		if _, retained := oldTerms[term]; !retained {
			idx.adjustDF(batch, ws, term, 1)
		}
	}
	for term := range oldTerms {
		if _, retained := newCounts[term]; !retained {
			idx.adjustDF(batch, ws, term, -1)
		}
	}

	err := batch.Commit(pebble.Sync)
	idx.mu.Unlock()
	return err
}

// adjustDF queues a document-frequency change of delta for term into batch.
// Caller must hold idx.mu — this reads the committed DF and writes back a
// derived value, so an unlocked concurrent caller loses the update (same reason
// IndexEngram takes the lock before its DF reads).
//
// A decrement is CLAMPED at 0 and skipped entirely when no term-stats entry
// exists. What an unclamped uint32 decrement costs, measured (getIDF ≡
// log((N−df+0.5)/(df+0.5)+1), and df=2^32−1 drives that ratio to just under −1):
//
//	WITH clamp:     df=0            getIDF(N=3) =   2.0794
//	WITHOUT clamp:  df=4294967295   getIDF(N=3) = -20.7944
//
// A NEGATIVE IDF is worse than a worthless one. COG-24's numerator is
// Σ idf_t'·cov_t, so a document that CONTAINS the term contributes a negative
// term and ranks BELOW one that does not: the term stops being weak evidence and
// becomes an active penalty against exactly the documents that hold it. Pinned by
// TestAdjustDF_DecrementClampsAtZero, which asserts both df==0 and idf>0 so a
// future change that bounds the DF but breaks the sign is still caught.
//
// There are two distinct ways df_t can be off, and the clamp guards the second:
//
//   - ABOVE its true value. DeleteEngram has never decremented DF, so an engram
//     deleted before ReindexEngram existed left df_t inflated relative to the
//     live corpus. Benign direction: an inflated df understates a term's rarity,
//     so a genuine match is under-credited, never over-credited.
//
//   - BELOW its true value. ReindexEngram is the FIRST decrementing DF path in the
//     codebase, so it is also the first way this can happen at all. Every
//     write-path initial index is an ftsWorker.Submit
//     (internal/engine/engine.go:1521 Write, :2063 the batch path, :3815 Evolve),
//     and Submit returns false when the queue is full (worker.go:145) or after
//     Stop (worker.go:133). An engram whose initial index job was DROPPED has no
//     postings, so its tag tokens were never counted — yet a later retag hands
//     ReindexEngram a prev containing them, and the −1 lands on a df another
//     engram legitimately owns. Measured:
//
//     before retag of unindexed B: df(zarquon)=1  (TRUE value = 1, only A has it)
//     after  retag of unindexed B: df(zarquon)=0  (TRUE value is still 1)
//
// So the clamp is not purely defensive — it bounds a reachable condition, and
// this is the path that made it reachable. The residual it leaves is bounded and
// mild by comparison: floored at 0, one-time per spurious decrement rather than a
// per-call ratchet, and it INFLATES the IDF of a term for the documents that do
// hold it rather than omitting them from results. `muninn reindex-fts` recomputes
// df_t from scratch and clears both directions.
func (idx *Index) adjustDF(batch *pebble.Batch, ws [8]byte, term string, delta int) {
	tkey := keys.TermStatsKey(ws, term)

	var currentDF uint32
	existed := false
	if val, closer, err := idx.db.Get(tkey); err == nil {
		if len(val) >= 4 {
			currentDF = binary.BigEndian.Uint32(val[0:4])
			existed = true
		}
		closer.Close()
	}

	switch {
	case delta > 0:
		currentDF += uint32(delta)
	case delta < 0:
		if !existed {
			return // nothing recorded, nothing to decrement
		}
		if d := uint32(-delta); currentDF <= d {
			currentDF = 0
		} else {
			currentDF -= d
		}
	default:
		return
	}

	var dfBuf [8]byte
	binary.BigEndian.PutUint32(dfBuf[:4], currentDF)
	batch.Set(tkey, dfBuf[:], nil)
}

// Search performs a calibrated full-text search for the given query string.
//
// The returned Score is an ABSOLUTE, query-calibrated coverage score in
// [0,1] — NOT raw BM25. It is computed as:
//
//	Score(d) = ( Σ_t idf_t' · cov_t(d) ) / ( Σ_t idf_t' )
//
// where t ranges over the query's (deduplicated, tokenized) content terms,
// cov_t(d) is term t's field-weighted BM25 coverage in doc d capped at 1
// (see coverageCap), and idf_t' is the term's IDF — except for a term with
// NO corpus term-stats entry at all (getIDF returns 0, meaning the corpus has
// literally never seen this term), which is charged at idfMax: the IDF a
// term would have at the maximum possible rarity (df=0) for this corpus size.
// An unseen term is the strongest possible evidence the vault knows nothing
// about the query, so it must penalize the score, not be skipped.
//
// The denominator depends ONLY on the query and the corpus's IDF statistics —
// never on the result set — so this is never a per-query-max normalization:
// a document's Score is identical no matter what else scored in the same
// search. See COG-24 (docs/internals/invariants.md) and issue #711.
//
// Before this calibration, Search summed raw unbounded BM25 across terms and
// the caller applied math.Tanh() to squash it into [0,1]. Raw BM25 saturates
// tanh by x≈3 (real magnitudes run 2-40), so a single common token in a
// high-weight field was indistinguishable from a genuine multi-term match —
// and corpus-absent query terms were silently skipped rather than penalizing.
// That silent skip + tanh saturation is what let a nonsense query with one
// coincidental common-word hit report full_text_relevance ≈ 0.9999.
func (idx *Index) Search(ctx context.Context, ws [8]byte, query string, topK int) ([]ScoredID, error) {
	start := time.Now()
	defer func() { metrics.FTSSearchDuration.Observe(time.Since(start).Seconds()) }()

	// Dual-path: search both stemmed tokens (new index) and unstemmed tokens (legacy index).
	// This ensures backward compatibility for vaults not yet re-indexed with stemming.
	stemmedTokens := Tokenize(query)
	rawTokens := tokenizeRaw(query)

	// Read global stats
	stats := idx.readStats(ws)

	// Determine whether to use raw-token fallback.
	// Vaults reindexed with ReindexFTSVault have FTSVersionKey=0x01 and skip the fallback.
	useRawFallback := true
	if cachedVer, ok := idx.versionCache.Load(ws); ok {
		useRawFallback = cachedVer.(byte) == 0x00
	} else {
		versionKey := keys.FTSVersionKey(ws)
		if val, closer, err := idx.db.Get(versionKey); err == nil {
			ver := val[0]
			closer.Close()
			idx.versionCache.Store(ws, ver)
			useRawFallback = ver == 0x00
		}
		// ErrNotFound means legacy vault — useRawFallback stays true
	}

	if len(stemmedTokens) == 0 {
		return nil, nil
	}
	N := float64(stats.TotalEngrams)
	avgdl := float64(stats.AvgDocLen)
	if avgdl <= 0 {
		avgdl = 1
	}

	// Guard against zero avgdl before the BM25 loop to prevent division by zero
	// in the b*dl/avgdl term, even if readStats returns a zero value.
	if avgdl <= 0 {
		avgdl = 1.0
	}

	// idfMax is the IDF a term would have at the maximum possible rarity
	// (df=0) for this corpus — the charge for a query term the corpus has
	// NEVER seen (getIDF returns 0 for "no TermStats entry", which is
	// exactly that case; it is distinct from a real, merely-low IDF).
	idfMax := math.Log((N+0.5)/0.5 + 1)

	// numer accumulates Σ_t idf_t'·cov_t(d) per engram; denom accumulates
	// Σ_t idf_t' — the query+corpus-only normalizer (never the result set).
	numer := make(map[[16]byte]float64, len(stemmedTokens)*20)
	var denom float64

	// stemmedTokens[i] and rawTokens[i] are the stemmed/unstemmed forms of the
	// SAME query word (Tokenize derives stemmedTokens from tokenizeRaw output
	// 1:1, in order — see Tokenize). Both forms are still searched independently
	// for real postings (a legacy, un-reindexed vault can have some engrams
	// indexed under the raw key and others under the stemmed key for the same
	// word — see TestFTS_DualPathSearch), so each real match still contributes
	// its own idf/coverage exactly as before #711. The ONLY change is: when
	// NEITHER form has any corpus stats at all (the word is genuinely absent),
	// the query word is charged idfMax exactly ONCE — not once per form —
	// otherwise every dual-path query on a legacy vault would double-penalize
	// absent words relative to a reindexed vault searching the same query.
	seenStem := make(map[string]bool, len(stemmedTokens))
	for i, stemmed := range stemmedTokens {
		if seenStem[stemmed] {
			continue // duplicate query word — already scored via its first occurrence
		}
		seenStem[stemmed] = true

		raw := rawTokens[i]
		tryRaw := useRawFallback && raw != stemmed

		idfStemmed := idx.getIDF(ws, stemmed, N)
		var idfRaw float64
		if tryRaw {
			idfRaw = idx.getIDF(ws, raw, N)
		}

		anyReal := false
		if idfStemmed > 0 {
			anyReal = true
			denom += idfStemmed
			_ = idx.searchTokenCoverage(ws, stemmed, numer, idfStemmed, avgdl)
		}
		if idfRaw > 0 {
			anyReal = true
			denom += idfRaw
			_ = idx.searchTokenCoverage(ws, raw, numer, idfRaw, avgdl)
		}
		if !anyReal {
			// Corpus-absent word (neither form has any stats): no postings
			// exist to scan, but it penalizes every candidate via the
			// denominator — charged exactly once for this query word.
			denom += idfMax
		}
	}

	if denom <= 0 {
		return nil, nil
	}

	results := make([]ScoredID, 0, len(numer))
	for id, n := range numer {
		results = append(results, ScoredID{ID: id, Score: n / denom})
	}
	sortScoredIDs(results)

	if topK > 0 && len(results) > topK {
		results = results[:topK]
	}
	return results, nil
}

// searchTokenCoverage performs a prefix scan for a single token and
// accumulates its IDF-weighted, capped coverage into numer[docID]:
//
//	numer[d] += idf * min(1, Σ_fields tfNorm·fieldWeight / (k1+1))
//
// The cap is applied per (term, doc) across ALL fields that term appears in
// within that doc — one term's field-weighted tf can't exceed a coverage of
// 1.0, so stuffing a single word cannot substitute for covering the query
// (the coverage this contributes is presence/prominence, not raw frequency).
func (idx *Index) searchTokenCoverage(ws [8]byte, term string, numer map[[16]byte]float64, idf, avgdl float64) error {
	// Prefix scan for this term across all fields.
	// Key format: 0x05 | ws[8] | term | 0x00 | field[1] | id[16]
	lowerBound := keys.FTSPostingKey(ws, term, 0x00, [16]byte{})
	upperBound := make([]byte, len(lowerBound))
	copy(upperBound, lowerBound)
	sepPos := 1 + 8 + len(term)
	upperBound[sepPos] = 0x01

	iter, err := idx.db.NewIter(&pebble.IterOptions{
		LowerBound: lowerBound,
		UpperBound: upperBound,
	})
	if err != nil {
		return err
	}
	defer iter.Close()

	minKeyLen := 1 + 8 + len(term) + 1 + 1 + 16
	idOffset := 1 + 8 + len(term) + 1 + 1

	// covRaw accumulates the uncapped, unweighted-by-idf coverage per doc for
	// this term (summed across every field the term appears in within that
	// doc) so the cap is applied ONCE per (term, doc), not per field.
	covRaw := make(map[[16]byte]float64, 32)

	for iter.First(); iter.Valid(); iter.Next() {
		key := iter.Key()
		if len(key) < minKeyLen {
			continue
		}
		var engramID [16]byte
		copy(engramID[:], key[idOffset:])

		val := iter.Value()
		pv := decodePosting(val)

		tf := float64(pv.TF)
		dl := float64(pv.DocLen)
		if dl < 1 {
			dl = avgdl
		}

		tfNorm := tf * (k1 + 1) / (tf + k1*(1-b+b*dl/avgdl))
		weighted := tfNorm * fieldWeight(pv.Field)
		if math.IsNaN(weighted) || math.IsInf(weighted, 0) {
			continue
		}
		covRaw[engramID] += weighted
	}

	for id, raw := range covRaw {
		cov := raw / (k1 + 1)
		if cov > 1 {
			cov = 1
		}
		numer[id] += idf * cov
	}
	return nil
}

// sortScoredIDs sorts in descending order by score.
func sortScoredIDs(s []ScoredID) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Score > s[j-1].Score; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// FTSStats holds global FTS statistics.
type FTSStats struct {
	TotalEngrams uint64
	AvgDocLen    float32
	VocabSize    uint64
}

// encodeStats encodes FTSStats to 20 bytes.
func encodeStats(st FTSStats) []byte {
	buf := make([]byte, 20)
	binary.BigEndian.PutUint64(buf[0:8], st.TotalEngrams)
	binary.BigEndian.PutUint32(buf[8:12], math.Float32bits(st.AvgDocLen))
	binary.BigEndian.PutUint64(buf[12:20], st.VocabSize)
	return buf
}

// decodeStats decodes 20 bytes into FTSStats.
func decodeStats(buf []byte) FTSStats {
	if len(buf) < 20 {
		return FTSStats{}
	}
	return FTSStats{
		TotalEngrams: binary.BigEndian.Uint64(buf[0:8]),
		AvgDocLen:    math.Float32frombits(binary.BigEndian.Uint32(buf[8:12])),
		VocabSize:    binary.BigEndian.Uint64(buf[12:20]),
	}
}

func (idx *Index) readStats(ws [8]byte) FTSStats {
	key := keys.FTSStatsKey(ws)
	val, closer, err := idx.db.Get(key)
	if err != nil {
		return FTSStats{TotalEngrams: 1, AvgDocLen: 100}
	}
	defer closer.Close()
	return decodeStats(val)
}

func (idx *Index) getIDF(ws [8]byte, term string, N float64) float64 {
	k := idfKey{ws, term}
	idx.mu.RLock()
	idf, ok := idx.idfCache[k]
	idx.mu.RUnlock()
	if ok {
		return idf
	}

	key := keys.TermStatsKey(ws, term)
	val, closer, err := idx.db.Get(key)
	if err != nil || len(val) < 8 {
		return 0
	}
	defer closer.Close()

	df := float64(binary.BigEndian.Uint32(val[0:4]))
	idf = math.Log((N-df+0.5)/(df+0.5) + 1)

	idx.mu.Lock()
	defer idx.mu.Unlock()
	// Double-check: another goroutine may have populated the cache while we
	// held no lock (between RUnlock above and this Lock).
	if cached, ok := idx.idfCache[k]; ok {
		return cached
	}
	idx.idfCache[k] = idf
	return idf
}

// UpdateStats increments the engram count and recalculates avgdl.
// The read-modify-write on the Pebble stats key is protected by idx.mu to prevent
// concurrent IndexEngram calls from producing a lost-update race.
func (idx *Index) UpdateStats(ws [8]byte, docLen int) error {
	key := keys.FTSStatsKey(ws)

	idx.mu.Lock()
	defer idx.mu.Unlock()

	val, closer, err := idx.db.Get(key)
	var st FTSStats
	if err == nil {
		st = decodeStats(val)
		closer.Close()
	}

	// Rolling average of doc length
	oldTotal := float64(st.TotalEngrams) * float64(st.AvgDocLen)
	st.TotalEngrams++
	st.AvgDocLen = float32((oldTotal + float64(docLen)) / float64(st.TotalEngrams))

	return idx.db.Set(key, encodeStats(st), pebble.NoSync)
}
