package storage

import "github.com/scrypster/muninndb/internal/types"

// DimFromLen maps a vector length (number of float32 elements, or quantized byte count)
// to an EmbedDimension enum value. Returns EmbedOther for any non-zero unknown length.
func DimFromLen(n int) types.EmbedDimension {
	switch n {
	case 0:
		return types.EmbedNone
	case 384:
		return types.Embed384
	case 768:
		return types.Embed768
	case 1536:
		return types.Embed1536
	case 3072:
		return types.Embed3072
	default:
		if n > 0 {
			return types.EmbedOther
		}
		return types.EmbedNone
	}
}
