package erf

import (
	"encoding/binary"
	"errors"
	"math"
	"time"
)

// PatchEmbedDim updates the EmbedDim byte in a raw ERF record in-place and
// recomputes the CRC32 trailer. Does NOT touch the CRC16 (covers bytes 0-5 only).
// raw must be a mutable copy of the 0x01 record (Get() already returns a copy).
func PatchEmbedDim(raw []byte, dim uint8) error {
	if len(raw) < VariableDataStart+TrailerSize {
		return errors.New("erf: record too short for PatchEmbedDim")
	}
	raw[OffsetEmbedDim] = dim
	crc32val := ComputeCRC32(raw[:len(raw)-TrailerSize])
	binary.BigEndian.PutUint32(raw[len(raw)-TrailerSize:], crc32val)
	return nil
}

// PatchRelevance updates Relevance, Stability, and UpdatedAt fields in a raw ERF record
// in-place. Recomputes the CRC32 trailer. Does NOT touch the CRC16 (covers bytes 0-5 only).
// raw must be a mutable copy of the 0x01 record (Get() already returns a copy).
func PatchRelevance(raw []byte, updatedAt time.Time, relevance, stability float32) error {
	if len(raw) < VariableDataStart+TrailerSize {
		return errors.New("erf: record too short for PatchRelevance")
	}
	binary.BigEndian.PutUint64(raw[OffsetUpdatedAt:OffsetUpdatedAt+8], uint64(updatedAt.UnixNano()))
	binary.BigEndian.PutUint32(raw[OffsetRelevance:OffsetRelevance+4], math.Float32bits(relevance))
	binary.BigEndian.PutUint32(raw[OffsetStability:OffsetStability+4], math.Float32bits(stability))
	crc32val := ComputeCRC32(raw[:len(raw)-TrailerSize])
	binary.BigEndian.PutUint32(raw[len(raw)-TrailerSize:], crc32val)
	return nil
}

// PatchAllMeta updates all mutable metadata fields in a raw ERF record in-place.
// Recomputes the CRC32 trailer. Does NOT touch the CRC16 (covers bytes 0-5 only).
// raw must be a mutable copy of the 0x01 record (Get() already returns a copy).
func PatchAllMeta(raw []byte, updatedAt, lastAccess time.Time, confidence, relevance, stability float32, accessCount uint32, state uint8) error {
	if len(raw) < VariableDataStart+TrailerSize {
		return errors.New("erf: record too short for PatchAllMeta")
	}
	binary.BigEndian.PutUint64(raw[OffsetUpdatedAt:OffsetUpdatedAt+8], uint64(updatedAt.UnixNano()))
	binary.BigEndian.PutUint64(raw[OffsetLastAccess:OffsetLastAccess+8], uint64(lastAccess.UnixNano()))
	binary.BigEndian.PutUint32(raw[OffsetConfidence:OffsetConfidence+4], math.Float32bits(confidence))
	binary.BigEndian.PutUint32(raw[OffsetRelevance:OffsetRelevance+4], math.Float32bits(relevance))
	binary.BigEndian.PutUint32(raw[OffsetStability:OffsetStability+4], math.Float32bits(stability))
	binary.BigEndian.PutUint32(raw[OffsetAccessCount:OffsetAccessCount+4], accessCount)
	raw[OffsetState] = state
	crc32val := ComputeCRC32(raw[:len(raw)-TrailerSize])
	binary.BigEndian.PutUint32(raw[len(raw)-TrailerSize:], crc32val)
	return nil
}
