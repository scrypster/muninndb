package keys

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"github.com/dchest/siphash"
	"golang.org/x/text/unicode/norm"
)

// SipHash keys for vault prefix computation
var (
	sipKey0 uint64 = 0x736f6d6570736575 // "somepseu"
	sipKey1 uint64 = 0x646f72616e646f6d // "dorandum"
)

// VaultPrefix computes the 8-byte SipHash prefix for a vault name.
func VaultPrefix(vault string) [8]byte {
	hashVal := siphash.Hash(sipKey0, sipKey1, []byte(vault))
	var prefix [8]byte
	binary.BigEndian.PutUint64(prefix[:], hashVal)
	return prefix
}

// EngramKey constructs the key for a full engram record (0x01 prefix).
// Key: 0x01 | wsPrefix(8) | ulid(16) = 25 bytes
func EngramKey(ws [8]byte, id [16]byte) []byte {
	key := make([]byte, 1+8+16)
	key[0] = 0x01
	copy(key[1:9], ws[:])
	copy(key[9:25], id[:])
	return key
}

// MetaKey constructs the key for metadata-only record (0x02 prefix).
func MetaKey(ws [8]byte, id [16]byte) []byte {
	key := make([]byte, 1+8+16)
	key[0] = 0x02
	copy(key[1:9], ws[:])
	copy(key[9:25], id[:])
	return key
}

// AssocFwdKey constructs the forward association key (0x03 prefix).
func AssocFwdKey(ws [8]byte, src [16]byte, weight float32, dst [16]byte) []byte {
	key := make([]byte, 1+8+16+4+16)
	key[0] = 0x03
	copy(key[1:9], ws[:])
	copy(key[9:25], src[:])
	wc := WeightComplement(weight)
	copy(key[25:29], wc[:])
	copy(key[29:45], dst[:])
	return key
}

// AssocRevKey constructs the reverse association key (0x04 prefix).
func AssocRevKey(ws [8]byte, dst [16]byte, weight float32, src [16]byte) []byte {
	key := make([]byte, 1+8+16+4+16)
	key[0] = 0x04
	copy(key[1:9], ws[:])
	copy(key[9:25], dst[:])
	wc := WeightComplement(weight)
	copy(key[25:29], wc[:])
	copy(key[29:45], src[:])
	return key
}

// FTSPostingKey constructs the FTS posting list entry key (0x05 prefix).
func FTSPostingKey(ws [8]byte, term string, id [16]byte) []byte {
	termBytes := []byte(term)
	key := make([]byte, 1+8+len(termBytes)+1+16)
	key[0] = 0x05
	copy(key[1:9], ws[:])
	copy(key[9:9+len(termBytes)], termBytes)
	key[9+len(termBytes)] = 0x00
	copy(key[10+len(termBytes):], id[:])
	return key
}

// TrigramKey constructs the trigram index key (0x06 prefix).
func TrigramKey(ws [8]byte, trigram [3]byte, id [16]byte) []byte {
	key := make([]byte, 1+8+3+16)
	key[0] = 0x06
	copy(key[1:9], ws[:])
	copy(key[9:12], trigram[:])
	copy(key[12:28], id[:])
	return key
}

// HNSWNodeKey constructs the HNSW node neighbor list key (0x07 prefix).
func HNSWNodeKey(ws [8]byte, id [16]byte, layer uint8) []byte {
	key := make([]byte, 1+8+16+1)
	key[0] = 0x07
	copy(key[1:9], ws[:])
	copy(key[9:25], id[:])
	key[25] = layer
	return key
}

// FTSStatsKey constructs the global FTS stats key (0x08 prefix).
func FTSStatsKey(ws [8]byte) []byte {
	key := make([]byte, 1+8+5)
	key[0] = 0x08
	copy(key[1:9], ws[:])
	copy(key[9:14], []byte("stats"))
	return key
}

// TermStatsKey constructs the per-term stats key (0x09 prefix).
func TermStatsKey(ws [8]byte, term string) []byte {
	termBytes := []byte(term)
	key := make([]byte, 1+8+len(termBytes))
	key[0] = 0x09
	copy(key[1:9], ws[:])
	copy(key[9:], termBytes)
	return key
}

// ContradictionKeyPrefix returns the 9-byte scan prefix for all contradictions in a vault.
func ContradictionKeyPrefix(ws [8]byte) []byte {
	key := make([]byte, 9)
	key[0] = 0x0A
	copy(key[1:9], ws[:])
	return key
}

// ContradictionKey constructs the contradiction index key (0x0A prefix).
func ContradictionKey(ws [8]byte, conceptHash uint32, relType uint16, id [16]byte) []byte {
	key := make([]byte, 1+8+4+2+16)
	key[0] = 0x0A
	copy(key[1:9], ws[:])
	binary.BigEndian.PutUint32(key[9:13], conceptHash)
	binary.BigEndian.PutUint16(key[13:15], relType)
	copy(key[15:31], id[:])
	return key
}

// StateIndexKey constructs the state secondary index key (0x0B prefix).
func StateIndexKey(ws [8]byte, state uint8, id [16]byte) []byte {
	key := make([]byte, 1+8+1+16)
	key[0] = 0x0B
	copy(key[1:9], ws[:])
	key[9] = state
	copy(key[10:26], id[:])
	return key
}

// TagIndexKey constructs the tag secondary index key (0x0C prefix).
func TagIndexKey(ws [8]byte, tagHash uint32, id [16]byte) []byte {
	key := make([]byte, 1+8+4+16)
	key[0] = 0x0C
	copy(key[1:9], ws[:])
	binary.BigEndian.PutUint32(key[9:13], tagHash)
	copy(key[13:29], id[:])
	return key
}

// CreatorIndexKey constructs the creator secondary index key (0x0D prefix).
func CreatorIndexKey(ws [8]byte, creatorHash uint32, id [16]byte) []byte {
	key := make([]byte, 1+8+4+16)
	key[0] = 0x0D
	copy(key[1:9], ws[:])
	binary.BigEndian.PutUint32(key[9:13], creatorHash)
	copy(key[13:29], id[:])
	return key
}

// VaultMetaKey constructs the vault metadata key (0x0E prefix).
// Value: human-readable vault name string.
// Key: 0x0E | wsPrefix(8) = 9 bytes
func VaultMetaKey(ws [8]byte) []byte {
	key := make([]byte, 1+8)
	key[0] = 0x0E
	copy(key[1:9], ws[:])
	return key
}

// VaultNameIndexKey constructs the forward vault-name index key (0x0F prefix).
// Keyed by the SipHash of the vault name so that any name resolves to its
// actual workspace prefix, even if the name is a legacy placeholder.
// Value: actual wsPrefix[8].
// Key: 0x0F | siphash(name)[8] = 9 bytes
func VaultNameIndexKey(name string) []byte {
	nameHash := siphash.Hash(sipKey0, sipKey1, []byte(name))
	key := make([]byte, 1+8)
	key[0] = 0x0F
	binary.BigEndian.PutUint64(key[1:], nameHash)
	return key
}

// RelevanceBucketKey constructs the relevance bucket index key (0x10 prefix).
// Key: 0x10 | wsPrefix(8) | storedBucket(1) | id(16) = 26 bytes
// storedBucket = uint8(9 - min(9, max(0, int(math.Floor(float64(relevance)*10)))))
// Higher relevance values produce lower bucket numbers (sort first in ascending scan).
func RelevanceBucketKey(ws [8]byte, relevance float32, id [16]byte) []byte {
	key := make([]byte, 1+8+1+16)
	key[0] = 0x10
	copy(key[1:9], ws[:])

	// Calculate storedBucket
	// relevance * 10 gives us a value from 0-10 (0-9 for valid range, clamped to 9)
	// floor of that gives us 0-9
	// min(9, max(0, floor)) clamps it to [0,9]
	// 9 - that value inverts it for descending sort (1.0 rel -> 0, 0.0 rel -> 9)
	floored := int(math.Floor(float64(relevance) * 10))
	clamped := floored
	if clamped < 0 {
		clamped = 0
	}
	if clamped > 9 {
		clamped = 9
	}
	storedBucket := uint8(9 - clamped)
	key[9] = storedBucket

	copy(key[10:26], id[:])
	return key
}

// DigestFlagsKey constructs the digest flags key (0x11 prefix) for an engram.
// Key: 0x11 | id(16) = 17 bytes (global — no vault scope needed since ULIDs are globally unique)
func DigestFlagsKey(id [16]byte) []byte {
	key := make([]byte, 1+16)
	key[0] = 0x11
	copy(key[1:17], id[:])
	return key
}

// CoherenceKey returns the 9-byte Pebble key for vault coherence counter persistence.
// Key layout: [0x12][8-byte vault prefix]
// Value layout: 56 bytes (7 × BigEndian int64)
func CoherenceKey(vaultPrefix [8]byte) []byte {
	key := make([]byte, 9)
	key[0] = 0x12
	copy(key[1:], vaultPrefix[:])
	return key
}

// VaultWeightsKey constructs the vault scoring-weights key (0x13 prefix).
// Key: 0x13 | wsPrefix(8) = 9 bytes
func VaultWeightsKey(ws [8]byte) []byte {
	key := make([]byte, 1+8)
	key[0] = 0x13
	copy(key[1:9], ws[:])
	return key
}

// AssocWeightIndexKey constructs the association weight index key (0x14 prefix).
// Stores the current float32 weight for an ordered pair (src, dst) for O(1)
// GetAssocWeight lookups without scanning the 0x03 forward key space.
// Key: 0x14 | wsPrefix(8) | src(16) | dst(16) = 41 bytes
func AssocWeightIndexKey(ws [8]byte, src [16]byte, dst [16]byte) []byte {
	key := make([]byte, 1+8+16+16)
	key[0] = 0x14
	copy(key[1:9], ws[:])
	copy(key[9:25], src[:])
	copy(key[25:41], dst[:])
	return key
}

// AssocFwdRangeStart returns the inclusive lower bound for scanning all forward
// associations within a vault (0x03 prefix scan lower bound).
func AssocFwdRangeStart(ws [8]byte) []byte {
	key := make([]byte, 1+8)
	key[0] = 0x03
	copy(key[1:9], ws[:])
	return key
}

// AssocFwdRangeEnd returns the exclusive upper bound for scanning all forward
// associations within a vault (increments the workspace prefix by 1 in the
// last byte, standard Pebble upper-bound idiom).
func AssocFwdRangeEnd(ws [8]byte) []byte {
	end := make([]byte, 1+8)
	end[0] = 0x03
	copy(end[1:9], ws[:])
	// Increment the last byte of ws portion in the key to get exclusive upper bound.
	for i := len(end) - 1; i >= 1; i-- {
		end[i]++
		if end[i] != 0 {
			break
		}
	}
	return end
}

// AssocFwdPrefixForID returns a 25-byte scan prefix covering all forward
// associations from a given source engram (0x03 | ws(8) | src(16)).
func AssocFwdPrefixForID(ws [8]byte, id [16]byte) []byte {
	key := make([]byte, 1+8+16)
	key[0] = 0x03
	copy(key[1:9], ws[:])
	copy(key[9:25], id[:])
	return key
}

// AssocRevPrefixForID returns a 25-byte scan prefix covering all reverse
// association index entries where the given engram is the target
// (0x04 | ws(8) | dstID(16)).
func AssocRevPrefixForID(ws [8]byte, id [16]byte) []byte {
	key := make([]byte, 1+8+16)
	key[0] = 0x04
	copy(key[1:9], ws[:])
	copy(key[9:25], id[:])
	return key
}

// VaultCountKey constructs the vault engram count key (0x15 prefix).
// Key: 0x15 | wsPrefix(8) = 9 bytes
// Value: BigEndian int64 total engram count for the vault.
//
// 0x15 is the sole user of this prefix. EpisodeKey uses 0x1A.
func VaultCountKey(ws [8]byte) []byte {
	key := make([]byte, 1+8)
	key[0] = 0x15
	copy(key[1:9], ws[:])
	return key
}

// ProvenanceKey constructs the provenance scan lower-bound key (0x16 prefix).
// Key: 0x16 | wsPrefix(8) | id(16) = 25 bytes
// Used as the lower bound for a prefix range scan over all provenance entries
// for a given engram.
func ProvenanceKey(ws [8]byte, id [16]byte) []byte {
	key := make([]byte, 1+8+16)
	key[0] = 0x16
	copy(key[1:9], ws[:])
	copy(key[9:25], id[:])
	return key
}

// ProvenanceKeyUpperBound constructs the exclusive upper bound for scanning all
// provenance entries of a given engram. It increments the id portion with carry-forward
// to handle the case where the last byte is 0xFF (a standard Pebble prefix upper-bound idiom).
func ProvenanceKeyUpperBound(ws [8]byte, id [16]byte) []byte {
	lower := ProvenanceKey(ws, id)
	upper := make([]byte, len(lower)+1) // +1 guarantees we include the full lower key
	copy(upper, lower)
	// Increment the id portion (bytes 9-24) with carry-forward.
	carried := true
	for i := len(lower) - 1; i >= 9; i-- {
		upper[i]++
		if upper[i] != 0 {
			upper = upper[:len(lower)]
			carried = false
			break
		}
	}
	if carried {
		// All bytes in the id wrapped to 0xFF; keep the +1 trailing 0x00 to ensure upper bound validity.
		copy(upper, lower)
	}
	return upper
}

// ProvenanceSuffixKey constructs a unique per-entry provenance key (0x16 prefix).
// Key: 0x16 | wsPrefix(8) | id(16) | timestamp_ns(8) | seq(4) = 37 bytes
// The BigEndian timestamp ensures chronological scan order.
func ProvenanceSuffixKey(ws [8]byte, id [16]byte, ts uint64, seq uint32) []byte {
	key := make([]byte, 1+8+16+8+4)
	key[0] = 0x16
	copy(key[1:9], ws[:])
	copy(key[9:25], id[:])
	binary.BigEndian.PutUint64(key[25:33], ts)
	binary.BigEndian.PutUint32(key[33:37], seq)
	return key
}

// EpisodeKey constructs the key for an episode record (0x1A prefix).
// Key: 0x1A | wsPrefix(8) | episodeID(16) = 25 bytes
func EpisodeKey(ws [8]byte, id [16]byte) []byte {
	key := make([]byte, 1+8+16)
	key[0] = 0x1A
	copy(key[1:9], ws[:])
	copy(key[9:25], id[:])
	return key
}

// EpisodeFrameKey constructs the key for an episode frame (0x1A prefix, with 0xFF separator).
// Key: 0x1A | wsPrefix(8) | episodeID(16) | 0xFF | position(4) = 30 bytes
func EpisodeFrameKey(ws [8]byte, episodeID [16]byte, position uint32) []byte {
	key := make([]byte, 1+8+16+1+4)
	key[0] = 0x1A
	copy(key[1:9], ws[:])
	copy(key[9:25], episodeID[:])
	key[25] = 0xFF
	binary.BigEndian.PutUint32(key[26:30], position)
	return key
}

// BucketMigrationKey constructs the bucket migration version key (0x17 prefix).
// Key: 0x17 | wsPrefix(8) = 9 bytes
// Value: [version uint8][optional cursor [16]byte] — used by MigrateBuckets.
func BucketMigrationKey(ws [8]byte) []byte {
	key := make([]byte, 1+8)
	key[0] = 0x17
	copy(key[1:9], ws[:])
	return key
}

// EmbeddingKey constructs the standalone embedding key (0x18 prefix) for ERF v2.
// Stores: 8-byte quantize params + N×int8 quantized bytes.
// Key: 0x18 | wsPrefix(8) | ulid(16) = 25 bytes
func EmbeddingKey(ws [8]byte, id [16]byte) []byte {
	key := make([]byte, 1+8+16)
	key[0] = 0x18
	copy(key[1:9], ws[:])
	copy(key[9:25], id[:])
	return key
}

// FTSVersionKey constructs the FTS schema version key (0x1B prefix).
// Key: 0x1B | wsPrefix(8) = 9 bytes
// Value: uint8 — 0 = legacy (unstemmed), 1 = re-indexed with Porter stemming.
// Once set to 1, dual-path query fallback is skipped (all tokens are stemmed).
func FTSVersionKey(ws [8]byte) []byte {
	key := make([]byte, 1+8)
	key[0] = 0x1B
	copy(key[1:9], ws[:])
	return key
}

// TransitionKey constructs the PAS transition table key (0x1C prefix).
// Key: 0x1C | wsPrefix(8) | srcID(16) | dstID(16) = 41 bytes
func TransitionKey(ws [8]byte, src [16]byte, dst [16]byte) []byte {
	key := make([]byte, 1+8+16+16)
	key[0] = 0x1C
	copy(key[1:9], ws[:])
	copy(key[9:25], src[:])
	copy(key[25:41], dst[:])
	return key
}

// TransitionPrefixForSrc returns a 25-byte scan prefix covering all transition
// targets from a given source engram (0x1C | ws(8) | src(16)).
func TransitionPrefixForSrc(ws [8]byte, src [16]byte) []byte {
	key := make([]byte, 1+8+16)
	key[0] = 0x1C
	copy(key[1:9], ws[:])
	copy(key[9:25], src[:])
	return key
}

// WeightComplement computes the weight complement for descending sort order.
func WeightComplement(weight float32) [4]byte {
	w := uint32(weight * float32(math.MaxUint32))
	c := uint32(math.MaxUint32) - w
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], c)
	return buf
}

// WeightFromComplement reconstructs the weight from its complement.
func WeightFromComplement(wc [4]byte) float32 {
	c := binary.BigEndian.Uint32(wc[:])
	w := uint32(math.MaxUint32) - c
	return float32(w) / float32(math.MaxUint32)
}

// EmbedModelKey constructs the vault-level embed model marker key (0x1D prefix).
// Key: 0x1D | wsPrefix(8) = 9 bytes
// Value: UTF-8 model name string. Empty/missing = not tracked.
func EmbedModelKey(ws [8]byte) []byte {
	key := make([]byte, 1+8)
	key[0] = 0x1D
	copy(key[1:9], ws[:])
	return key
}

// OrdinalKey constructs the ordinal index key (0x1E prefix).
// Stores the sibling position (ordinal) of childID within parentID.
// Key: 0x1E | wsPrefix(8) | parentID(16) | childID(16) = 41 bytes
func OrdinalKey(ws [8]byte, parentID [16]byte, childID [16]byte) []byte {
	key := make([]byte, 1+8+16+16)
	key[0] = 0x1E
	copy(key[1:9], ws[:])
	copy(key[9:25], parentID[:])
	copy(key[25:41], childID[:])
	return key
}

// OrdinalPrefixForParent returns a 25-byte scan prefix covering all child ordinals
// under a given parent engram (0x1E | ws(8) | parentID(16)).
// Used by ListChildOrdinals to scan all children of a parent.
func OrdinalPrefixForParent(ws [8]byte, parentID [16]byte) []byte {
	key := make([]byte, 1+8+16)
	key[0] = 0x1E
	copy(key[1:9], ws[:])
	copy(key[9:25], parentID[:])
	return key
}

// OrdinalWorkspacePrefix returns a 9-byte scan prefix covering ALL ordinal keys
// in a workspace (0x1E | ws(8)). Used by DeleteEngram to find all ordinal entries
// where the deleted engram is a child.
func OrdinalWorkspacePrefix(ws [8]byte) []byte {
	key := make([]byte, 1+8)
	key[0] = 0x1E
	copy(key[1:9], ws[:])
	return key
}

// IncrementWSPrefix returns the next workspace prefix for use as an exclusive
// upper bound in Pebble range operations.
func IncrementWSPrefix(ws [8]byte) ([8]byte, error) {
	result := ws
	for i := 7; i >= 0; i-- {
		result[i]++
		if result[i] != 0 {
			return result, nil
		}
	}
	return [8]byte{}, fmt.Errorf("workspace prefix overflow")
}

// Hash computes a 32-bit FNV-1a hash for string tags/creators.
func Hash(s string) uint32 {
	h := uint32(2166136261)
	for _, c := range []byte(s) {
		h ^= uint32(c)
		h *= 16777619
	}
	return h
}

// EntityNameHash computes the 8-byte SipHash of a NFKC-normalized, lowercased,
// trimmed entity name. Used for the 0x1F entity key and 0x20 link key.
func EntityNameHash(name string) [8]byte {
	normalized := strings.ToLower(strings.TrimSpace(norm.NFKC.String(name)))
	hashVal := siphash.Hash(sipKey0, sipKey1, []byte(normalized))
	var h [8]byte
	binary.BigEndian.PutUint64(h[:], hashVal)
	return h
}

// EntityKey constructs the global entity record key (0x1F prefix).
// Key: 0x1F | nameHash(8) = 9 bytes
func EntityKey(nameHash [8]byte) []byte {
	key := make([]byte, 1+8)
	key[0] = 0x1F
	copy(key[1:9], nameHash[:])
	return key
}

// EntityEngramLinkKey constructs the engram→entity link key (0x20 prefix).
// Key: 0x20 | wsPrefix(8) | engramID(16) | nameHash(8) = 33 bytes
func EntityEngramLinkKey(ws [8]byte, engramID [16]byte, nameHash [8]byte) []byte {
	key := make([]byte, 1+8+16+8)
	key[0] = 0x20
	copy(key[1:9], ws[:])
	copy(key[9:25], engramID[:])
	copy(key[25:33], nameHash[:])
	return key
}

// EntityEngramLinkPrefix returns a 25-byte prefix for scanning all entity links
// from a given engram (0x20 | ws(8) | engramID(16)).
func EntityEngramLinkPrefix(ws [8]byte, engramID [16]byte) []byte {
	key := make([]byte, 1+8+16)
	key[0] = 0x20
	copy(key[1:9], ws[:])
	copy(key[9:25], engramID[:])
	return key
}

// RelationshipKey constructs a vault-scoped relationship key (0x21 prefix).
// Key: 0x21 | ws(8) | engramID(16) | fromNameHash(8) | relTypeByte(1) | toNameHash(8) = 42 bytes
func RelationshipKey(ws [8]byte, engramID [16]byte, fromHash [8]byte, relTypeByte uint8, toHash [8]byte) []byte {
	key := make([]byte, 1+8+16+8+1+8)
	key[0] = 0x21
	copy(key[1:9], ws[:])
	copy(key[9:25], engramID[:])
	copy(key[25:33], fromHash[:])
	key[33] = relTypeByte
	copy(key[34:42], toHash[:])
	return key
}

// RelationshipPrefix returns the 9-byte scan prefix for all relationship records
// in a given vault (0x21 | wsPrefix(8)).
func RelationshipPrefix(ws [8]byte) []byte {
	key := make([]byte, 1+8)
	key[0] = 0x21
	copy(key[1:9], ws[:])
	return key
}

// CoOccurrenceKey constructs the entity co-occurrence index key (0x24 prefix).
// Tracks how many times two entities appear in the same engram within a vault.
// Key: 0x24 | wsPrefix(8) | nameHashA(8) | nameHashB(8) = 25 bytes
// Always stored with nameHashA <= nameHashB (canonical pair order).
// Value: msgpack(coOccurrenceRecord{NameA, NameB, Count uint32}).
func CoOccurrenceKey(ws [8]byte, hashA, hashB [8]byte) []byte {
	key := make([]byte, 1+8+8+8)
	key[0] = 0x24
	copy(key[1:9], ws[:])
	copy(key[9:17], hashA[:])
	copy(key[17:25], hashB[:])
	return key
}

// CoOccurrencePrefix returns the 9-byte scan prefix for all co-occurrence entries
// in a given vault (0x24 | wsPrefix(8)).
func CoOccurrencePrefix(ws [8]byte) []byte {
	key := make([]byte, 1+8)
	key[0] = 0x24
	copy(key[1:9], ws[:])
	return key
}

// EntityReverseIndexKey constructs the entity→engram reverse index key (0x23 prefix).
// Enables "which engrams mention entity X?" queries by scanning 0x23|nameHash prefix.
// Key: 0x23 | nameHash(8) | wsPrefix(8) | engramID(16) = 33 bytes
// Value: empty (all data is encoded in the key).
func EntityReverseIndexKey(nameHash [8]byte, ws [8]byte, engramID [16]byte) []byte {
	key := make([]byte, 1+8+8+16)
	key[0] = 0x23
	copy(key[1:9], nameHash[:])
	copy(key[9:17], ws[:])
	copy(key[17:33], engramID[:])
	return key
}

// EntityReverseIndexPrefix returns a 9-byte prefix for scanning all engrams
// that mention a given entity (0x23 | nameHash(8)).
func EntityReverseIndexPrefix(nameHash [8]byte) []byte {
	key := make([]byte, 1+8)
	key[0] = 0x23
	copy(key[1:9], nameHash[:])
	return key
}

// LastAccessIndexKey constructs the LastAccess index key (0x22 prefix).
// Uses inverted milliseconds (^uint64(unixMillis)) so ascending Pebble scan
// returns most-recently-accessed entries first.
// Key: 0x22 | wsPrefix(8) | invertedMillis(8) | engramID(16) = 33 bytes
// Value: empty (all data is in the key).
func LastAccessIndexKey(ws [8]byte, lastAccessMillis int64, engramID [16]byte) []byte {
	key := make([]byte, 1+8+8+16)
	key[0] = 0x22
	copy(key[1:9], ws[:])
	inverted := ^uint64(lastAccessMillis)
	binary.BigEndian.PutUint64(key[9:17], inverted)
	copy(key[17:33], engramID[:])
	return key
}

// LastAccessIndexPrefix returns the 9-byte prefix for scanning all LastAccess
// entries in a vault (0x22 | ws(8)). Ascending scan yields most-recently-accessed first.
func LastAccessIndexPrefix(ws [8]byte) []byte {
	key := make([]byte, 1+8)
	key[0] = 0x22
	copy(key[1:9], ws[:])
	return key
}

// IdempotencyKey constructs the global idempotency receipt key (0x19 prefix).
// Uses SipHash of the op_id string (same SipHash params as EntityNameHash).
// Key: 0x19 | siphash(op_id)(8) = 9 bytes
// Value: JSON {"engram_id": "...", "created_at": unix_nanos}
func IdempotencyKey(opID string) []byte {
	hashVal := siphash.Hash(sipKey0, sipKey1, []byte(opID))
	key := make([]byte, 1+8)
	key[0] = 0x19
	binary.BigEndian.PutUint64(key[1:], hashVal)
	return key
}

// ArchiveAssocKey constructs the archived association key (0x25 prefix).
// No weight complement — archive keys are not sorted by weight.
// No reverse key — restore is one-directional (BFS always traverses outbound edges).
// Key: 0x25 | wsPrefix(8) | src(16) | dst(16) = 41 bytes
func ArchiveAssocKey(ws [8]byte, src [16]byte, dst [16]byte) []byte {
	key := make([]byte, 1+8+16+16)
	key[0] = 0x25
	copy(key[1:9], ws[:])
	copy(key[9:25], src[:])
	copy(key[25:41], dst[:])
	return key
}

// ArchiveAssocPrefixForID returns a 25-byte scan prefix covering all archived
// associations from a given source engram (0x25 | ws(8) | src(16)).
func ArchiveAssocPrefixForID(ws [8]byte, src [16]byte) []byte {
	key := make([]byte, 1+8+16)
	key[0] = 0x25
	copy(key[1:9], ws[:])
	copy(key[9:25], src[:])
	return key
}

// ArchiveAssocRangeStart returns the inclusive lower bound for scanning all
// archived associations within a vault (0x25 | ws(8)).
func ArchiveAssocRangeStart(ws [8]byte) []byte {
	key := make([]byte, 1+8)
	key[0] = 0x25
	copy(key[1:9], ws[:])
	return key
}

// ArchiveAssocRangeEnd returns the exclusive upper bound for scanning all
// archived associations within a vault. Increments the ws portion.
func ArchiveAssocRangeEnd(ws [8]byte) []byte {
	end := make([]byte, 1+8)
	end[0] = 0x25
	copy(end[1:9], ws[:])
	for i := len(end) - 1; i >= 1; i-- {
		end[i]++
		if end[i] != 0 {
			break
		}
	}
	return end
}
