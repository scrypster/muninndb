package embed

import "math"

// clsPool extracts the CLS token embedding from a flat hidden-state tensor.
// BGE models (bge-small-en-v1.5) encode sentence meaning into the [CLS] token.
// hidden: flat [seqLen, dim] slice for a single sequence.
func clsPool(hidden []float32, dim int) []float32 {
	result := make([]float32, dim)
	copy(result, hidden[:dim])
	return result
}

// meanPool computes the mean of token embeddings weighted by the attention mask.
// hidden: flat [1, seqLen, dim] slice; mask: [seqLen] with 0/1 values.
func meanPool(hidden []float32, mask []int64, seqLen, dim int) []float32 {
	result := make([]float32, dim)
	var count float32
	for t := 0; t < seqLen; t++ {
		if mask[t] == 0 {
			continue
		}
		count++
		base := t * dim
		for d := 0; d < dim; d++ {
			result[d] += hidden[base+d]
		}
	}
	if count > 0 {
		for d := range result {
			result[d] /= count
		}
	}
	return result
}

// l2Normalize divides v by its L2 norm in place.
func l2Normalize(v []float32) {
	var sum float32
	for _, x := range v {
		sum += x * x
	}
	if sum == 0 {
		return
	}
	inv := float32(1.0 / math.Sqrt(float64(sum)))
	for i := range v {
		v[i] *= inv
	}
}
