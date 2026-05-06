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

	FieldConcept    = uint8(0x01)
	FieldTags       = uint8(0x02)
	FieldContent    = uint8(0x03)
	FieldCreatedBy  = uint8(0x04)

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
type Index struct {
	db       *pebble.DB
	mu       sync.RWMutex
	// In-memory IDF cache: term → idf
	idfCache map[string]float64
	// versionCache caches the FTS schema version per vault (0=legacy dual-path, 1=stemmed-only).
	// Populated lazily on first Search() for each vault; FTSVersionKey is write-once.
	versionCache sync.Map // key: [8]byte wsPrefix, value: byte
}

func New(db *pebble.DB) *Index {
	return &Index{
		db:       db,
		idfCache: make(map[string]float64, 1024),
	}
}

// InvalidateIDFCache clears the in-memory IDF cache, forcing fresh recalculation
// on the next search. Call this after a vault clear to prevent stale IDF values
// from influencing BM25 scoring.
func (idx *Index) InvalidateIDFCache() {
	idx.mu.Lock()
	idx.idfCache = make(map[string]float64)
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
		delete(idx.idfCache, term)
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
		delete(idx.idfCache, term)
	}

	err := batch.Commit(pebble.Sync)
	idx.mu.Unlock()
	return err
}

// Search performs a BM25 search for the given query string.
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

	// Union: include both token forms when they differ
	allTokens := make([]string, 0, len(stemmedTokens)*2)
	seen := make(map[string]struct{})
	// Always include stemmed tokens
	for _, t := range stemmedTokens {
		if _, exists := seen[t]; !exists {
			allTokens = append(allTokens, t)
			seen[t] = struct{}{}
		}
	}
	// Only include raw tokens for legacy vaults (dual-path backward compat)
	if useRawFallback {
		for _, t := range rawTokens {
			if _, exists := seen[t]; !exists {
				allTokens = append(allTokens, t)
				seen[t] = struct{}{}
			}
		}
	}
	tokens := allTokens

	if len(tokens) == 0 {
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

	// Per-engram accumulated scores; pre-allocate based on token count to reduce rehash overhead.
	scores := make(map[[16]byte]float64, len(tokens)*20)

	for _, term := range tokens {
		idf := idx.getIDF(ws, term, N)
		if idf <= 0 {
			continue
		}
		_ = idx.searchToken(ws, term, scores, idf, avgdl)
	}

	// Sort by score descending
	results := make([]ScoredID, 0, len(scores))
	for id, score := range scores {
		results = append(results, ScoredID{ID: id, Score: score})
	}
	sortScoredIDs(results)

	if topK > 0 && len(results) > topK {
		results = results[:topK]
	}
	return results, nil
}

// searchToken performs a prefix scan for a single token and accumulates BM25
// scores into the provided scores map. Extracting this into its own function
// ensures that defer iter.Close() is scoped to the function lifetime rather
// than the Search() loop body, which would otherwise defer all closes until
// Search() returns (and risk double-close on the last iterator).
func (idx *Index) searchToken(ws [8]byte, term string, scores map[[16]byte]float64, idf, avgdl float64) error {
	// Prefix scan for this term across all fields.
	// Key format: 0x05 | ws[8] | term | 0x00 | field[1] | id[16]
	// Use field=0x00 for lower bound; upper bound stops at the 0x00 separator + 0x01.
	lowerBound := keys.FTSPostingKey(ws, term, 0x00, [16]byte{})
	// Upper bound: increment the separator byte after the term so the scan covers
	// all field values (0x00–0xFF) for this term.
	upperBound := make([]byte, len(lowerBound))
	copy(upperBound, lowerBound)
	// sepPos is the index of the 0x00 separator byte: prefix(1) + ws(8) + term.
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

	// Key layout: 0x05[1] | ws[8] | term[n] | 0x00[1] | field[1] | id[16]
	minKeyLen := 1 + 8 + len(term) + 1 + 1 + 16
	idOffset := 1 + 8 + len(term) + 1 + 1 // skip prefix, ws, term, separator, field

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

		// BM25 formula
		tfNorm := tf * (k1 + 1) / (tf + k1*(1-b+b*dl/avgdl))
		bm25 := idf * tfNorm * fieldWeight(pv.Field)

		// Guard against NaN/Inf scores that corrupt sorting
		if math.IsNaN(bm25) || math.IsInf(bm25, 0) {
			continue
		}

		scores[engramID] += bm25
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
	idx.mu.RLock()
	idf, ok := idx.idfCache[term]
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
	if cached, ok := idx.idfCache[term]; ok {
		return cached
	}
	idx.idfCache[term] = idf
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
