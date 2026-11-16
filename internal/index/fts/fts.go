package fts

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/cockroachdb/pebble"
	"github.com/kljensen/snowball"
	"github.com/scrypster/muninndb/internal/metrics"
	"github.com/scrypster/muninndb/internal/rankorder"
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

// Index is the FTS inverted index backed by Pebble.
//
// It holds no derived scoring state. A search takes one snapshot and works out
// everything — the corpus size, each word's IDF, each posting list — from that
// one committed state, so its scores are a function of a state the index was
// actually in and of the query, and of nothing this process did earlier (see
// scoreAll for the snapshot and getIDF for why no memo is kept). Writers keep
// their half of that: a document's postings, document frequencies and corpus
// statistics commit in one batch.
type Index struct {
	db *pebble.DB
	// mu serializes the read-modify-write of the document-frequency and corpus
	// stats keys across writers. Searches do not take it
	// (TestSearch_CompletesWhileAWriterHoldsTheIndexLock).
	mu sync.RWMutex
	// versionCache caches the FTS schema version per vault (0=legacy dual-path, 1=stemmed-only).
	// Populated lazily on first Search() for each vault; FTSVersionKey is write-once.
	versionCache sync.Map // key: [8]byte wsPrefix, value: byte
	// statsBehindWarned rate-limits reportStatsBehind's WARN to one per vault per
	// statsBehindWarnInterval. key: [8]byte wsPrefix, value: *atomic.Int64 (unix nanos).
	statsBehindWarned sync.Map
}

// statsBehindWarnInterval is how often, per vault, a search may log that the
// vault's keyword statistics are inconsistent. The counter is not rate-limited.
const statsBehindWarnInterval = 10 * time.Minute

func New(db *pebble.DB) *Index {
	return &Index{db: db}
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

// HasPostings reports whether the engram whose document is doc has its
// keyword postings in the index. One point read: postings are written in one
// batch per engram, so any one of them stands for all. A document with no
// indexable terms reports true (there is nothing to be missing).
func (idx *Index) HasPostings(ws [8]byte, id [16]byte, doc Document) (bool, error) {
	counts, _ := docTermCounts(doc.Concept, doc.CreatedBy, doc.Content, doc.Tags)
	for term, fields := range counts {
		for field := range fields {
			_, closer, err := idx.db.Get(keys.FTSPostingKey(ws, term, field, id))
			if err == pebble.ErrNotFound {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			closer.Close()
			return true, nil
		}
	}
	return true, nil
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
	defer batch.Close()

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

		// Read current DF and write updated DF into the same batch. A read
		// that FAILS is not a count of zero: writing 1 over a count that could
		// not be read would durably reset the word's frequency.
		currentDF, _, err := readDF(idx.db, ws, term)
		if err != nil {
			idx.mu.Unlock()
			return err
		}
		var dfBuf [8]byte
		binary.BigEndian.PutUint32(dfBuf[:4], currentDF+1)
		batch.Set(keys.TermStatsKey(ws, term), dfBuf[:], nil)
	}

	// The corpus statistics (document count, average length) ride in the same
	// batch. Written separately, as they were, every index left a window — and
	// every crash in it a permanent state — where a word's frequency counted a
	// document the corpus size did not.
	st, err := idx.statsWithDocument(ws, int(docLen))
	if err != nil {
		idx.mu.Unlock()
		return err
	}
	batch.Set(keys.FTSStatsKey(ws), encodeStats(st), nil)

	// Commit single atomic batch: postings, DF updates and corpus statistics
	// land together.
	err = batch.Commit(pebble.Sync)
	idx.mu.Unlock()
	return err
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
// under-credited, never over-credited — and `muninn vault reindex-fts <name>` recomputes
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
	}

	// DF adjustments: membership changes only.
	for term := range newCounts {
		if _, retained := oldTerms[term]; !retained {
			if err := idx.adjustDF(batch, ws, term, 1); err != nil {
				idx.mu.Unlock()
				return err
			}
		}
	}
	for term := range oldTerms {
		if _, retained := newCounts[term]; !retained {
			if err := idx.adjustDF(batch, ws, term, -1); err != nil {
				idx.mu.Unlock()
				return err
			}
		}
	}

	err := batch.Commit(pebble.Sync)
	idx.mu.Unlock()
	return err
}

// readDF returns term's committed document frequency. found is false when the
// vault holds no record for the term — the only case that means the corpus has
// never counted it. Any other read failure is returned as an error and never
// folded into "not found": the search path would report a word the vault holds
// as absent, and the write paths, which read this to add or subtract one, would
// commit a count derived from nothing over the real one.
func readDF(r pebble.Reader, ws [8]byte, term string) (df uint32, found bool, err error) {
	val, closer, err := r.Get(keys.TermStatsKey(ws, term))
	if errors.Is(err, pebble.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("fts: read document frequency: %w", err)
	}
	defer closer.Close()
	if len(val) < 4 {
		return 0, false, nil
	}
	return binary.BigEndian.Uint32(val[0:4]), true, nil
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
// (Those two figures predate getIDF clamping N up to df. With that clamp a
// wrapped df gives an IDF of about 1e-10, not −20.79: the word would carry no
// weight rather than be read as absent. Still wrong, still this clamp's job to
// prevent, and the df==0 half of that test is what pins it now.)
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
// hold it rather than omitting them from results. `muninn vault reindex-fts <name>` recomputes
// df_t from scratch and clears both directions.
func (idx *Index) adjustDF(batch *pebble.Batch, ws [8]byte, term string, delta int) error {
	currentDF, existed, err := readDF(idx.db, ws, term)
	if err != nil {
		return err
	}

	switch {
	case delta > 0:
		currentDF += uint32(delta)
	case delta < 0:
		if !existed {
			return nil // nothing recorded, nothing to decrement
		}
		if d := uint32(-delta); currentDF <= d {
			currentDF = 0
		} else {
			currentDF -= d
		}
	default:
		return nil
	}

	var dfBuf [8]byte
	binary.BigEndian.PutUint32(dfBuf[:4], currentDF)
	batch.Set(keys.TermStatsKey(ws, term), dfBuf[:], nil)
	return nil
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

	results, err := idx.scoreAll(ws, query)
	if err != nil || results == nil {
		return results, err
	}
	if topK > 0 && len(results) > topK {
		results = results[:topK]
	}
	return results, nil
}

// SearchLive is Search for a vault holding RETIRED documents — superseded
// versions whose postings are kept on purpose (COG-28: they are the keyword
// path a query worded like an OLD version still reaches; dropping them, as
// forget does for trash, would make that wording unable to find the current
// fact at all). Search cuts the full score-sorted match list at topK, so
// retired versions that out-score live documents take their slots; SearchLive
// cuts the LIVE matches at topK and returns the best shadowBudget retired
// matches separately, on a budget not counted against topK.
//
// No over-fetch is needed: BM25 coverage scoring already scores every
// matching document before the cut, and each document's score is its own
// (no cross-document normalisation), so skipping retired ones at the cut
// changes no live score. Corpus statistics (N, df) still include retired
// documents, exactly as before — a separate question from slot consumption.
func (idx *Index) SearchLive(ctx context.Context, ws [8]byte, query string, topK int, retired func([16]byte) bool, shadowBudget int) (live, shadows []ScoredID, err error) {
	start := time.Now()
	defer func() { metrics.FTSSearchDuration.Observe(time.Since(start).Seconds()) }()

	results, err := idx.scoreAll(ws, query)
	if err != nil || results == nil {
		return results, nil, err
	}
	live = results[:0:0]
	for _, r := range results {
		if retired != nil && retired(r.ID) {
			if len(shadows) < shadowBudget {
				shadows = append(shadows, r)
			}
			continue
		}
		if topK > 0 && len(live) >= topK {
			if len(shadows) >= shadowBudget {
				break
			}
			continue
		}
		live = append(live, r)
	}
	return live, shadows, nil
}

// scoreAll scores every document matching query and returns them
// score-descending (sortScoredIDs order), uncut. nil when nothing can match.
func (idx *Index) scoreAll(ws [8]byte, query string) ([]ScoredID, error) {
	// Dual-path: search both stemmed tokens (new index) and unstemmed tokens (legacy index).
	// This ensures backward compatibility for vaults not yet re-indexed with stemming.
	stemmedTokens := Tokenize(query)
	rawTokens := tokenizeRaw(query)

	if len(stemmedTokens) == 0 {
		return nil, nil
	}

	// Every read below goes through ONE snapshot: the document count, every
	// word's document frequency and every posting list are read as of the same
	// committed state. Read separately they can straddle a write, and a score
	// then mixes two states of the index and matches neither (measured before
	// the snapshot: 339 of 383,404 scores for one document under a concurrent
	// writer matched no state the store was ever in, the worst 0.9745 against a
	// highest legitimate 0.9279).
	//
	// And every read that FAILS fails the search. A failed read is not "no
	// match": reporting it as one hands the caller a confident empty result for
	// a query the vault may well answer, with nothing to tell it apart from a
	// real miss.
	snap := idx.db.NewSnapshot()
	defer snap.Close()
	var r pebble.Reader = snap

	// Read global stats
	stats, err := idx.readStats(r, ws)
	if err != nil {
		return nil, err
	}

	// Determine whether to use raw-token fallback.
	// Vaults reindexed with ReindexFTSVault have FTSVersionKey=0x01 and skip the fallback.
	useRawFallback := true
	if cachedVer, ok := idx.versionCache.Load(ws); ok {
		useRawFallback = cachedVer.(byte) == 0x00
	} else {
		val, closer, verr := r.Get(keys.FTSVersionKey(ws))
		switch {
		case verr == nil:
			if len(val) > 0 {
				ver := val[0]
				idx.versionCache.Store(ws, ver)
				useRawFallback = ver == 0x00
			}
			closer.Close()
		case errors.Is(verr, pebble.ErrNotFound):
			// legacy vault — useRawFallback stays true
		default:
			return nil, fmt.Errorf("fts: read schema version: %w", verr)
		}
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

		idfStemmed, err := idx.getIDF(r, ws, stemmed, N)
		if err != nil {
			return nil, err
		}
		var idfRaw float64
		if tryRaw {
			if idfRaw, err = idx.getIDF(r, ws, raw, N); err != nil {
				return nil, err
			}
		}

		anyReal := false
		if idfStemmed > 0 {
			anyReal = true
			denom += idfStemmed
			if err := idx.searchTokenCoverage(r, ws, stemmed, numer, idfStemmed, avgdl); err != nil {
				return nil, err
			}
		}
		if idfRaw > 0 {
			anyReal = true
			denom += idfRaw
			if err := idx.searchTokenCoverage(r, ws, raw, numer, idfRaw, avgdl); err != nil {
				return nil, err
			}
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
func (idx *Index) searchTokenCoverage(r pebble.Reader, ws [8]byte, term string, numer map[[16]byte]float64, idf, avgdl float64) error {
	// Prefix scan for this term across all fields.
	// Key format: 0x05 | ws[8] | term | 0x00 | field[1] | id[16]
	lowerBound := keys.FTSPostingKey(ws, term, 0x00, [16]byte{})
	upperBound := make([]byte, len(lowerBound))
	copy(upperBound, lowerBound)
	sepPos := 1 + 8 + len(term)
	upperBound[sepPos] = 0x01

	iter, err := r.NewIter(&pebble.IterOptions{
		LowerBound: lowerBound,
		UpperBound: upperBound,
	})
	if err != nil {
		return fmt.Errorf("fts: scan postings: %w", err)
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
	// An iterator that stopped on a read error looks exactly like one that ran
	// out of postings. Check before crediting anything: a partial scan must not
	// score as if it were the whole posting list.
	if err := iter.Error(); err != nil {
		return fmt.Errorf("fts: scan postings: %w", err)
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

// sortScoredIDs sorts in descending order by score, breaking exact ties
// newest-first (rankorder.Before: ULID descending). The tie-break is
// load-bearing twice over. Search builds its input from a Go map, and coverage
// is capped per term, so many documents routinely tie exactly: sorting on score
// alone left tied documents in map-iteration order, so which of them survived
// the topK cut — the recall candidate SET — changed between identical calls.
// And this cut runs BEFORE recall's final sort, so its direction must be the
// final sort's: an oldest-first cut here kept the oldest k of a set of
// identical memories and the newest never became candidates at all.
func sortScoredIDs(s []ScoredID) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Score != s[j].Score {
			return s[i].Score > s[j].Score
		}
		return rankorder.Before(s[i].ID, s[j].ID)
	})
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

// readStats returns the vault's corpus statistics. A vault with no statistics
// record yet reads as one document of average length 100, the long-standing
// placeholder for an empty index. Any other read failure is an error.
func (idx *Index) readStats(r pebble.Reader, ws [8]byte) (FTSStats, error) {
	val, closer, err := r.Get(keys.FTSStatsKey(ws))
	if errors.Is(err, pebble.ErrNotFound) {
		return FTSStats{TotalEngrams: 1, AvgDocLen: 100}, nil
	}
	if err != nil {
		return FTSStats{}, fmt.Errorf("fts: read corpus statistics: %w", err)
	}
	defer closer.Close()
	return decodeStats(val), nil
}

// getIDF returns term's IDF in the vault for a corpus of N documents, or 0 when
// the vault has no document-frequency record for it. scoreAll reads 0 as "the
// corpus has never counted this word" and charges it at idfMax.
//
// That is a statement about the RECORD, not about the word. A word can have
// postings and no record: a memory whose first index never ran, later restored
// or retagged, gets its postings from ReindexEngram with no frequency for the
// words it already had. Such a word returns 0 here and matches nothing although
// a document holds it. Nothing in this function can tell; the write path has to
// stop producing that state.
//
// The IDF is computed on every call from the committed document frequency. It
// used to be memoized per (vault, term), and that memo was the bug: an IDF is a
// function of BOTH N and df, the memo was dropped only when a document holding
// the term was indexed, and it was filled outside the lock from two unrelated
// reads. So a value computed while a write was in flight outlived that write,
// and a value computed in a small corpus was still served in a large one. The
// memo saved one point read per query word, against a posting scan per word.
//
// N is clamped up to df. Every document counted in df is a document of the
// corpus, so df > N means N is behind. A search no longer meets that in passing
// — it reads N and df from one snapshot, and IndexEngram commits both in one
// batch — so when it happens the statistics on disk disagree, and
// reportStatsBehind says so. Unclamped, df = N+1 makes the logarithm negative
// for a term every document holds, and a non-positive IDF is indistinguishable
// from "never counted": the term's postings are not scanned and it is charged as
// absent. With the clamp a term that has a record always has a positive, finite
// IDF, non-increasing in df and non-decreasing in N
// (TestGetIDF_PositiveFiniteAndMonotone).
//
// The clamp fixes the sign, not the size. With N one behind, a word nearly every
// document holds has an IDF up to about 3x off and a query made of such words a
// keyword score up to 0.25 off — 40 documents, two words held by 39 and 40 of
// them: a document holding only the second scores 0.248 settled and 0.494 with N
// one behind.
//
// Measured on the build that had the memo, from an empty data directory, 40
// invented memories per vault written over MCP by one to four builders at once,
// the first 8 sharing a tag: the tag word ended with no full-text match in 12 of
// 820 vault builds and with a stale positive IDF in 3 more. The on-disk index
// was complete in every one; the state lasted until a restart, a reindex, or
// another write holding the word. Independently reproduced with a real writer
// and a searcher that never pauses: 300 of 300 trials with the memo, 0 of 300
// without.
func (idx *Index) getIDF(r pebble.Reader, ws [8]byte, term string, N float64) (float64, error) {
	rawDF, found, err := readDF(r, ws, term)
	if err != nil || !found {
		return 0, err
	}

	df := float64(rawDF)
	if N < df {
		idx.reportStatsBehind(ws, rawDF, N)
		N = df
	}
	return math.Log((N-df+0.5)/(df+0.5) + 1), nil
}

// reportStatsBehind records that a search found a word counted in more
// documents than the vault's corpus statistics say exist. A search reads both
// numbers from one snapshot and IndexEngram commits both in one batch, so this
// is not a race: the statistics on disk disagree, and will until they are
// rebuilt. The search still answers, with N clamped (see getIDF); this is what
// makes the condition visible instead of merely survivable.
//
// Known ways to get here that are not corruption: a store last written by a
// build that committed the document count in a separate, unsynced write and
// crashed in between; and a memory whose first index never ran and that was
// then edited in place, which adds to a word's frequency through ReindexEngram
// without adding to the document count.
//
// The word itself is not logged: it is memory content.
func (idx *Index) reportStatsBehind(ws [8]byte, df uint32, n float64) {
	metrics.FTSStatsInconsistent.Inc()

	v, _ := idx.statsBehindWarned.LoadOrStore(ws, new(atomic.Int64))
	last := v.(*atomic.Int64)
	now := time.Now().UnixNano()
	prev := last.Load()
	if prev != 0 && now-prev < int64(statsBehindWarnInterval) {
		return
	}
	if !last.CompareAndSwap(prev, now) {
		return // another search just logged it
	}
	slog.Warn("fts: keyword statistics are inconsistent — a word is counted in more documents than the vault holds; scores use a clamped count until `muninn vault reindex-fts <vault>` rebuilds them",
		"vault_prefix", fmt.Sprintf("%x", ws), "document_frequency", df, "documents", uint64(n))
}

// statsWithDocument returns the vault's corpus statistics as they stand after
// one more document of docLen tokens. Caller holds idx.mu: this is the read half
// of a read-modify-write.
func (idx *Index) statsWithDocument(ws [8]byte, docLen int) (FTSStats, error) {
	// Not found means an empty index. A read that FAILED must not: carrying on
	// would write a document count of 1 over the real one.
	val, closer, err := idx.db.Get(keys.FTSStatsKey(ws))
	var st FTSStats
	switch {
	case err == nil:
		st = decodeStats(val)
		closer.Close()
	case !errors.Is(err, pebble.ErrNotFound):
		return FTSStats{}, fmt.Errorf("fts: read corpus statistics: %w", err)
	}

	// Rolling average of doc length
	oldTotal := float64(st.TotalEngrams) * float64(st.AvgDocLen)
	st.TotalEngrams++
	st.AvgDocLen = float32((oldTotal + float64(docLen)) / float64(st.TotalEngrams))
	return st, nil
}

// UpdateStats counts one more document of docLen tokens in the vault's corpus
// statistics, on its own. IndexEngram does not use it — it commits the same
// update inside its posting batch — so this is for a caller adding to the count
// without indexing through IndexEngram.
func (idx *Index) UpdateStats(ws [8]byte, docLen int) error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	st, err := idx.statsWithDocument(ws, docLen)
	if err != nil {
		return err
	}
	return idx.db.Set(keys.FTSStatsKey(ws), encodeStats(st), pebble.NoSync)
}
