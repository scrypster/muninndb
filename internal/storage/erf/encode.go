package erf

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/vmihailenco/msgpack/v5"
)

// Encode serializes an Engram into ERF v1 binary format.
func Encode(eng *Engram) ([]byte, error) {
	b := GetBuffer()
	defer PutBuffer(b)

	if err := encodeInto(b, eng); err != nil {
		return nil, err
	}

	result := make([]byte, len(b.buf))
	copy(result, b.buf)
	return result, nil
}

func encodeInto(b *erfBuffer, eng *Engram) error {
	// Validate field size limits
	if len(eng.Concept) > MaxConceptBytes {
		return fmt.Errorf("erf: concept exceeds max length %d (got %d)", MaxConceptBytes, len(eng.Concept))
	}
	if len(eng.CreatedBy) > MaxCreatedByBytes {
		return fmt.Errorf("erf: created_by exceeds max length %d (got %d)", MaxCreatedByBytes, len(eng.CreatedBy))
	}
	if len(eng.Content) > MaxContentBytes {
		return fmt.Errorf("erf: content exceeds max length %d (got %d)", MaxContentBytes, len(eng.Content))
	}

	b.buf = append(b.buf, make([]byte, FixedOverhead)...)

	flags := uint8(0)
	if len(eng.Embedding) > 0 {
		flags |= (1 << 0)
	}

	binary.BigEndian.PutUint32(b.buf[0:4], Magic)
	b.buf[4] = Version

	copy(b.buf[OffsetID:OffsetID+16], eng.ID[:])
	binary.BigEndian.PutUint64(b.buf[OffsetCreatedAt:OffsetCreatedAt+8], uint64(eng.CreatedAt.UnixNano()))
	binary.BigEndian.PutUint64(b.buf[OffsetUpdatedAt:OffsetUpdatedAt+8], uint64(eng.UpdatedAt.UnixNano()))
	binary.BigEndian.PutUint64(b.buf[OffsetLastAccess:OffsetLastAccess+8], uint64(eng.LastAccess.UnixNano()))
	binary.BigEndian.PutUint32(b.buf[OffsetConfidence:OffsetConfidence+4], math.Float32bits(eng.Confidence))
	binary.BigEndian.PutUint32(b.buf[OffsetRelevance:OffsetRelevance+4], math.Float32bits(eng.Relevance))
	binary.BigEndian.PutUint32(b.buf[OffsetStability:OffsetStability+4], math.Float32bits(eng.Stability))
	binary.BigEndian.PutUint32(b.buf[OffsetAccessCount:OffsetAccessCount+4], eng.AccessCount)
	b.buf[OffsetState] = eng.State
	binary.BigEndian.PutUint16(b.buf[OffsetAssocCount:OffsetAssocCount+2], uint16(len(eng.Associations)))
	b.buf[OffsetEmbedDim] = eng.EmbedDim
	b.buf[OffsetMemoryType] = eng.MemoryType
	binary.BigEndian.PutUint16(b.buf[OffsetClassification:OffsetClassification+2], eng.Classification)

	conceptBytes := []byte(eng.Concept)
	createdByBytes := []byte(eng.CreatedBy)
	contentBytes := []byte(eng.Content)

	if len(contentBytes) > ContentCompressThreshold {
		compressed, wasCompressed := Compress(contentBytes)
		if wasCompressed {
			contentBytes = compressed
			flags |= (1 << 1)
		}
	}

	tagsBytes, err := msgpack.Marshal(eng.Tags)
	if err != nil {
		return err
	}

	assocBytes := make([]byte, 0, len(eng.Associations)*AssocRecordSize)
	for i := range eng.Associations {
		assocBuf := make([]byte, AssocRecordSize)
		if err := EncodeAssociation(assocBuf, &eng.Associations[i]); err != nil {
			return err
		}
		assocBytes = append(assocBytes, assocBuf...)
	}

	var embedBytes []byte
	if len(eng.Embedding) > 0 {
		params, quantized := Quantize(eng.Embedding)
		flags |= (1 << 2)
		paramsBuf := EncodeQuantizeParams(params)
		embedBytes = append(embedBytes, paramsBuf[:]...)
		for _, v := range quantized {
			embedBytes = append(embedBytes, byte(v))
		}
	}

	// Validate total variable data size does not exceed uint32 max
	totalVarSize := uint64(len(conceptBytes)) + uint64(len(createdByBytes)) + uint64(len(contentBytes)) + uint64(len(tagsBytes)) + uint64(len(assocBytes)) + uint64(len(embedBytes))
	if totalVarSize > math.MaxUint32 {
		return fmt.Errorf("erf: variable data too large (total: %d bytes, max: %d)", totalVarSize, uint32(math.MaxUint32))
	}

	varStart := VariableDataStart
	conceptOff := uint32(varStart)
	conceptLen := uint16(len(conceptBytes))
	createdByOff := uint32(varStart + len(conceptBytes))
	createdByLen := uint16(len(createdByBytes))
	contentOff := uint32(varStart + len(conceptBytes) + len(createdByBytes))
	contentLen := uint32(len(contentBytes))
	tagsOff := uint32(varStart + len(conceptBytes) + len(createdByBytes) + len(contentBytes))
	tagsLen := uint32(len(tagsBytes))
	assocOff := uint32(varStart + len(conceptBytes) + len(createdByBytes) + len(contentBytes) + len(tagsBytes))
	assocLen := uint32(len(assocBytes))
	embedOff := uint32(varStart + len(conceptBytes) + len(createdByBytes) + len(contentBytes) + len(tagsBytes) + len(assocBytes))
	embedLen := uint32(len(embedBytes))

	binary.BigEndian.PutUint32(b.buf[OffsetConceptOff:OffsetConceptOff+4], conceptOff)
	binary.BigEndian.PutUint16(b.buf[OffsetConceptLen:OffsetConceptLen+2], conceptLen)
	binary.BigEndian.PutUint32(b.buf[OffsetCreatedByOff:OffsetCreatedByOff+4], createdByOff)
	binary.BigEndian.PutUint16(b.buf[OffsetCreatedByLen:OffsetCreatedByLen+2], createdByLen)
	binary.BigEndian.PutUint32(b.buf[OffsetContentOff:OffsetContentOff+4], contentOff)
	binary.BigEndian.PutUint32(b.buf[OffsetContentLen:OffsetContentLen+4], contentLen)
	binary.BigEndian.PutUint32(b.buf[OffsetTagsOff:OffsetTagsOff+4], tagsOff)
	binary.BigEndian.PutUint32(b.buf[OffsetTagsLen:OffsetTagsLen+4], tagsLen)
	binary.BigEndian.PutUint32(b.buf[OffsetAssocOff:OffsetAssocOff+4], assocOff)
	binary.BigEndian.PutUint32(b.buf[OffsetAssocLen:OffsetAssocLen+4], assocLen)
	binary.BigEndian.PutUint32(b.buf[OffsetEmbedOff:OffsetEmbedOff+4], embedOff)
	binary.BigEndian.PutUint32(b.buf[OffsetEmbedLen:OffsetEmbedLen+4], embedLen)

	b.buf = append(b.buf, conceptBytes...)
	b.buf = append(b.buf, createdByBytes...)
	b.buf = append(b.buf, contentBytes...)
	b.buf = append(b.buf, tagsBytes...)
	b.buf = append(b.buf, assocBytes...)
	b.buf = append(b.buf, embedBytes...)

	// Tagged extension fields (backward compatible: old decoders skip unknown tags)
	if eng.TypeLabel != "" {
		b.buf = appendTaggedString(b.buf, TagTypeLabel, eng.TypeLabel)
	}
	if eng.Summary != "" {
		b.buf = appendTaggedString(b.buf, TagSummary, eng.Summary)
	}
	if len(eng.KeyPoints) > 0 {
		if kpBytes, err := msgpack.Marshal(eng.KeyPoints); err == nil {
			b.buf = appendTaggedBytes(b.buf, TagKeyPoints, kpBytes)
		}
	}

	b.buf[5] = flags

	crc16 := ComputeCRC16(b.buf[0:6])
	binary.BigEndian.PutUint16(b.buf[6:8], crc16)

	crc32 := ComputeCRC32(b.buf)
	b.buf = append(b.buf, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(b.buf[len(b.buf)-4:], crc32)

	return nil
}

// EncodeV2 serializes an Engram into ERF v2 binary format.
// V2 does NOT write inline associations or embeddings — both live in separate Pebble keys.
// Callers are responsible for writing the embedding to the 0x18 key separately.
func EncodeV2(eng *Engram) ([]byte, error) {
	b := GetBuffer()
	defer PutBuffer(b)

	if err := encodeV2Into(b, eng); err != nil {
		return nil, err
	}

	result := make([]byte, len(b.buf))
	copy(result, b.buf)
	return result, nil
}

func encodeV2Into(b *erfBuffer, eng *Engram) error {
	if len(eng.Concept) > MaxConceptBytes {
		return fmt.Errorf("erf: concept exceeds max length %d (got %d)", MaxConceptBytes, len(eng.Concept))
	}
	if len(eng.CreatedBy) > MaxCreatedByBytes {
		return fmt.Errorf("erf: created_by exceeds max length %d (got %d)", MaxCreatedByBytes, len(eng.CreatedBy))
	}
	if len(eng.Content) > MaxContentBytes {
		return fmt.Errorf("erf: content exceeds max length %d (got %d)", MaxContentBytes, len(eng.Content))
	}

	b.buf = append(b.buf, make([]byte, FixedOverhead)...)

	flags := uint8(0)
	binary.BigEndian.PutUint32(b.buf[0:4], Magic)
	b.buf[4] = Version2 // v2 — no inline assoc/embed

	copy(b.buf[OffsetID:OffsetID+16], eng.ID[:])
	binary.BigEndian.PutUint64(b.buf[OffsetCreatedAt:OffsetCreatedAt+8], uint64(eng.CreatedAt.UnixNano()))
	binary.BigEndian.PutUint64(b.buf[OffsetUpdatedAt:OffsetUpdatedAt+8], uint64(eng.UpdatedAt.UnixNano()))
	binary.BigEndian.PutUint64(b.buf[OffsetLastAccess:OffsetLastAccess+8], uint64(eng.LastAccess.UnixNano()))
	binary.BigEndian.PutUint32(b.buf[OffsetConfidence:OffsetConfidence+4], math.Float32bits(eng.Confidence))
	binary.BigEndian.PutUint32(b.buf[OffsetRelevance:OffsetRelevance+4], math.Float32bits(eng.Relevance))
	binary.BigEndian.PutUint32(b.buf[OffsetStability:OffsetStability+4], math.Float32bits(eng.Stability))
	binary.BigEndian.PutUint32(b.buf[OffsetAccessCount:OffsetAccessCount+4], eng.AccessCount)
	b.buf[OffsetState] = eng.State
	// AssocCount and EmbedDim are zero for v2 (zero-initialized by make above)
	b.buf[OffsetMemoryType] = eng.MemoryType
	binary.BigEndian.PutUint16(b.buf[OffsetClassification:OffsetClassification+2], eng.Classification)

	conceptBytes := []byte(eng.Concept)
	createdByBytes := []byte(eng.CreatedBy)
	contentBytes := []byte(eng.Content)

	if len(contentBytes) > ContentCompressThreshold {
		compressed, wasCompressed := Compress(contentBytes)
		if wasCompressed {
			contentBytes = compressed
			flags |= FlagContentCompressed
		}
	}

	tagsBytes, err := msgpack.Marshal(eng.Tags)
	if err != nil {
		return err
	}

	// No assoc or embed bytes for v2
	varStart := VariableDataStart
	conceptOff := uint32(varStart)
	conceptLen := uint16(len(conceptBytes))
	createdByOff := uint32(varStart + len(conceptBytes))
	createdByLen := uint16(len(createdByBytes))
	contentOff := uint32(varStart + len(conceptBytes) + len(createdByBytes))
	contentLen := uint32(len(contentBytes))
	tagsOff := uint32(varStart + len(conceptBytes) + len(createdByBytes) + len(contentBytes))
	tagsLen := uint32(len(tagsBytes))

	binary.BigEndian.PutUint32(b.buf[OffsetConceptOff:OffsetConceptOff+4], conceptOff)
	binary.BigEndian.PutUint16(b.buf[OffsetConceptLen:OffsetConceptLen+2], conceptLen)
	binary.BigEndian.PutUint32(b.buf[OffsetCreatedByOff:OffsetCreatedByOff+4], createdByOff)
	binary.BigEndian.PutUint16(b.buf[OffsetCreatedByLen:OffsetCreatedByLen+2], createdByLen)
	binary.BigEndian.PutUint32(b.buf[OffsetContentOff:OffsetContentOff+4], contentOff)
	binary.BigEndian.PutUint32(b.buf[OffsetContentLen:OffsetContentLen+4], contentLen)
	binary.BigEndian.PutUint32(b.buf[OffsetTagsOff:OffsetTagsOff+4], tagsOff)
	binary.BigEndian.PutUint32(b.buf[OffsetTagsLen:OffsetTagsLen+4], tagsLen)
	// AssocOff/Len and EmbedOff/Len remain zero (zero-initialized above)

	b.buf = append(b.buf, conceptBytes...)
	b.buf = append(b.buf, createdByBytes...)
	b.buf = append(b.buf, contentBytes...)
	b.buf = append(b.buf, tagsBytes...)

	// Tagged extension fields (backward compatible: old decoders skip unknown tags)
	if eng.TypeLabel != "" {
		b.buf = appendTaggedString(b.buf, TagTypeLabel, eng.TypeLabel)
	}
	if eng.Summary != "" {
		b.buf = appendTaggedString(b.buf, TagSummary, eng.Summary)
	}
	if len(eng.KeyPoints) > 0 {
		if kpBytes, err := msgpack.Marshal(eng.KeyPoints); err == nil {
			b.buf = appendTaggedBytes(b.buf, TagKeyPoints, kpBytes)
		}
	}

	b.buf[5] = flags

	crc16 := ComputeCRC16(b.buf[0:6])
	binary.BigEndian.PutUint16(b.buf[6:8], crc16)

	crc32 := ComputeCRC32(b.buf)
	b.buf = append(b.buf, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(b.buf[len(b.buf)-4:], crc32)

	return nil
}
