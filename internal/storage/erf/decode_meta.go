package erf

import (
	"encoding/binary"
	"errors"
	"math"
	"time"
)

// DecodeMetaConcept decodes the metadata fields and the concept string from a
// MetaKeySize-byte slim value stored under the 0x02 metadata key.
// Returns (*EngramMeta, concept, error).
func DecodeMetaConcept(data []byte) (*EngramMeta, string, error) {
	meta, err := DecodeMeta(data)
	if err != nil {
		return nil, "", err
	}

	if len(data) < VariableDataStart {
		return meta, "", nil
	}

	// Read concept offset and length from the offset table.
	if len(data) < OffsetConceptLen+2 {
		return meta, "", nil
	}
	conceptOff := binary.BigEndian.Uint32(data[OffsetConceptOff : OffsetConceptOff+4])
	conceptLen := binary.BigEndian.Uint16(data[OffsetConceptLen : OffsetConceptLen+2])

	end := int(conceptOff) + int(conceptLen)
	if int(conceptOff) < VariableDataStart || end > len(data) {
		return meta, "", nil
	}
	concept := string(data[conceptOff:end])
	return meta, concept, nil
}

// DecodeMeta decodes only the first 108 bytes (header + metadata) into EngramMeta.
// This is the fast path used by decay/tier workers and activation scoring.
func DecodeMeta(data []byte) (*EngramMeta, error) {
	if len(data) < OffsetTablePos {
		return nil, errors.New("data too short for metadata decode")
	}

	if !VerifyCRC16(data[0:8]) {
		return nil, errors.New("metadata crc16 check failed")
	}

	meta := &EngramMeta{}
	copy(meta.ID[:], data[OffsetID:OffsetID+16])
	meta.CreatedAt = time.Unix(0, int64(binary.BigEndian.Uint64(data[OffsetCreatedAt:OffsetCreatedAt+8])))
	meta.UpdatedAt = time.Unix(0, int64(binary.BigEndian.Uint64(data[OffsetUpdatedAt:OffsetUpdatedAt+8])))
	meta.LastAccess = time.Unix(0, int64(binary.BigEndian.Uint64(data[OffsetLastAccess:OffsetLastAccess+8])))
	meta.Confidence = math.Float32frombits(binary.BigEndian.Uint32(data[OffsetConfidence : OffsetConfidence+4]))
	meta.Relevance = math.Float32frombits(binary.BigEndian.Uint32(data[OffsetRelevance : OffsetRelevance+4]))
	meta.Stability = math.Float32frombits(binary.BigEndian.Uint32(data[OffsetStability : OffsetStability+4]))
	meta.AccessCount = binary.BigEndian.Uint32(data[OffsetAccessCount : OffsetAccessCount+4])
	meta.State = data[OffsetState]
	meta.AssocCount = binary.BigEndian.Uint16(data[OffsetAssocCount : OffsetAssocCount+2])
	meta.EmbedDim = data[OffsetEmbedDim]
	meta.MemoryType = data[OffsetMemoryType]
	meta.Trust = data[OffsetTrust]
	meta.ValidFrom, meta.ValidUntil = decodeValidity(data, meta.CreatedAt)
	meta.Importance = math.Float32frombits(binary.BigEndian.Uint32(data[OffsetImportance : OffsetImportance+4]))

	return meta, nil
}
