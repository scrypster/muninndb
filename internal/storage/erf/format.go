package erf

// ERF v1 constants: magic bytes, version, offsets, sizes
const (
	Magic    uint32 = 0x4D554E4E // "MUNN"
	Version  uint8  = 0x01
	Version2 uint8  = 0x02

	// Section offsets
	HeaderOffset    = 0
	HeaderSize      = 8
	MetadataOffset  = 8
	MetadataSize    = 100
	OffsetTablePos  = 108
	OffsetTableSize = 40
	FixedOverhead   = 152 // HeaderSize + MetadataSize + OffsetTableSize + TrailerSize
	TrailerSize     = 4

	// Metadata field offsets (relative to record start)
	OffsetID          = 8
	OffsetCreatedAt   = 24
	OffsetUpdatedAt   = 32
	OffsetLastAccess  = 40
	OffsetConfidence  = 48
	OffsetRelevance   = 52
	OffsetStability   = 56
	OffsetAccessCount = 60
	OffsetState       = 64
	OffsetAssocCount  = 65
	OffsetEmbedDim    = 67
	OffsetMemoryType  = 68 // uint8, first byte of formerly-reserved area
	OffsetClassification = 69 // uint16, big-endian
	OffsetReserved    = 71 // 29 bytes reserved (was 32)

	// Offset table field offsets (relative to record start)
	OffsetConceptOff   = 108
	OffsetConceptLen   = 112
	OffsetCreatedByOff = 114
	OffsetCreatedByLen = 118
	OffsetContentOff   = 120
	OffsetContentLen   = 124
	OffsetTagsOff      = 128
	OffsetTagsLen      = 132
	OffsetAssocOff     = 136
	OffsetAssocLen     = 140
	OffsetEmbedOff     = 144
	OffsetEmbedLen     = 148

	// Variable data starts at offset 152 (after offset table)
	VariableDataStart = 152

	// Association sub-record size
	AssocRecordSize = 40

	// Limits
	MaxConceptBytes           = 512
	MaxCreatedByBytes         = 64
	MaxContentBytes           = 16 * 1024 // 16KB
	ContentCompressThreshold  = 512       // zstd compress content > this size

	// CRC
	CRC16Polynomial = 0x1021 // CRC-16/CCITT-FALSE

	// MetaKeySize is the maximum number of bytes from the start of a full ERF record
	// stored in the 0x02 metadata-only Pebble key. Includes the fixed overhead plus
	// enough variable data to hold the concept field (always the first variable field).
	// Use MetaKeySlice(data) rather than data[:MetaKeySize] — the record may be shorter.
	MetaKeySize = VariableDataStart + MaxConceptBytes // 664 bytes
)

// MetaKeySlice returns the prefix of data to store in the 0x02 metadata key.
// Safe when len(data) < MetaKeySize (e.g. short concepts).
func MetaKeySlice(data []byte) []byte {
	n := MetaKeySize
	if n > len(data) {
		n = len(data)
	}
	return data[:n]
}

// ERF flag byte constants (offset 5 in the record).
const (
	FlagHasEmbedding      uint8 = 1 << 0
	FlagContentCompressed uint8 = 1 << 1
	FlagEmbedQuantized    uint8 = 1 << 2
	FlagDormant           uint8 = 1 << 3
	FlagSoftDeleted       uint8 = 1 << 4
	FlagDirty             uint8 = 1 << 5
)

// Tagged extension field prefixes. Written after variable data, before CRC32 trailer.
// Format: tag(1) | len(2, big-endian) | data(len).
const (
	TagTypeLabel  uint8 = 0x19 // free-form TypeLabel string
	TagSummary    uint8 = 0x1A // abstractive summary (UTF-8 string)
	TagKeyPoints  uint8 = 0x1B // semantic key points (msgpack []string)
)

// appendTaggedString appends a tagged length-prefixed UTF-8 string to buf.
func appendTaggedString(buf []byte, tag uint8, s string) []byte {
	data := []byte(s)
	if len(data) > 0xFFFF {
		data = data[:0xFFFF]
	}
	buf = append(buf, tag)
	buf = append(buf, byte(len(data)>>8), byte(len(data)))
	buf = append(buf, data...)
	return buf
}

// appendTaggedBytes appends a tagged length-prefixed raw byte slice to buf.
func appendTaggedBytes(buf []byte, tag uint8, data []byte) []byte {
	if len(data) > 0xFFFF {
		data = data[:0xFFFF]
	}
	buf = append(buf, tag)
	buf = append(buf, byte(len(data)>>8), byte(len(data)))
	buf = append(buf, data...)
	return buf
}
