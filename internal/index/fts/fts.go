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

// IndexEngram writes FTS posting list entries for an engram.
// ws is the 8-byte workspace prefix. id is the ULID.
func (idx *Index) IndexEngram(ws [8]byte, id [16]byte, concept, createdBy, content string, tags []string) error {
	// Collect all (term, field, docLen) tuples
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

	// Total doc len for BM25 normalization — must include all indexed fields.
	allTokens := Tokenize(concept + " " + content + " " + createdBy + " " + strings.Join(tags, " "))
	docLen := uint16(len(allTokens))

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
