package scoring

import (
	"math"
	"time"
)

// Dimension indices for the 6-dimensional weight vector.
const (
	DimFTS         = 0 // full-text search score
	DimHNSW        = 1 // vector similarity score
	DimHebbian     = 2 // Hebbian association boost
	DimDecay       = 3 // decay/recency factor
	DimRecency     = 4 // creation recency
	DimAssociation = 5 // graph association weight
	NumDims        = 6
)

// VaultWeights is the per-vault learned scoring weight vector.
type VaultWeights struct {
	VaultPrefix  [8]byte
	Weights      [NumDims]float64
	LearningRate float64
	UpdateCount  int64
	UpdatedAt    time.Time
}

// FeedbackSignal encodes implicit feedback from access patterns.
type FeedbackSignal struct {
	EngramID    [16]byte
	Accessed    bool             // true = positive signal (retrieved AND accessed within 5 min)
	ScoreVector [NumDims]float64 // the score components that produced this result
	Timestamp   time.Time
}

// DefaultWeights returns equal weights (1/N each) with softmax normalization.
func DefaultWeights() [NumDims]float64 {
	w := [NumDims]float64{}
	for i := 0; i < NumDims; i++ {
		w[i] = 1.0 / float64(NumDims)
	}
	return Softmax(w)
}

// Softmax normalizes the weight vector so values sum to 1.
// Returns normalized copy — does not modify in place.
// If result contains NaN, returns uniform distribution (1/N for each dim).
func Softmax(w [NumDims]float64) [NumDims]float64 {
	// Shift by max for numerical stability
	maxW := w[0]
	for i := 1; i < NumDims; i++ {
		if w[i] > maxW {
			maxW = w[i]
		}
	}

	var sum float64
	shifted := [NumDims]float64{}
	for i := 0; i < NumDims; i++ {
		exp := math.Exp(w[i] - maxW)
		shifted[i] = exp
		sum += exp
	}

	var result [NumDims]float64
	for i := 0; i < NumDims; i++ {
		result[i] = shifted[i] / sum
	}

	// Check for NaN and reset to uniform distribution if found
	hasNaN := false
	for i := 0; i < NumDims; i++ {
		if math.IsNaN(result[i]) {
			hasNaN = true
			break
		}
	}
	if hasNaN {
		uniform := 1.0 / float64(NumDims)
		for i := 0; i < NumDims; i++ {
			result[i] = uniform
		}
	}

	return result
}

// Update applies a stochastic gradient descent step.
// Positive signal: increase weights for dimensions that scored high.
// Negative signal: decrease weights for dimensions that scored high.
// Applies minimum weight floor of 0.05 per dimension after update.
// Applies Softmax normalization after floor clamping.
// Skips gradient application if gradient is NaN or Inf.
func (vw *VaultWeights) Update(signal FeedbackSignal) {
	if vw.LearningRate <= 0 {
		vw.LearningRate = 0.1 // default learning rate
	}

	const minFloor = 0.05

	// Gradient direction: positive signal increases, negative decreases
	direction := 1.0
	if !signal.Accessed {
		direction = -1.0
	}

	// Apply SGD: w_new = w_old + lr * direction * score_vector (normalized)
	for i := 0; i < NumDims; i++ {
		gradient := vw.LearningRate * direction * signal.ScoreVector[i]
		// Skip gradient application if it's NaN or Inf
		if math.IsNaN(gradient) || math.IsInf(gradient, 0) {
			continue
		}
		vw.Weights[i] += gradient
	}

	// Clamp to floor to prevent any dimension from becoming negligible
	for i := 0; i < NumDims; i++ {
		if vw.Weights[i] < minFloor {
			vw.Weights[i] = minFloor
		}
	}

	// Normalize via softmax so weights sum to 1
	vw.Weights = Softmax(vw.Weights)

	// Update metadata
	vw.UpdateCount++
	vw.UpdatedAt = signal.Timestamp
}

// Blend blends this vault's weights with a parent vault's weights.
// result = 0.7 * self + 0.3 * parent
func (vw *VaultWeights) Blend(parent [NumDims]float64) [NumDims]float64 {
	const selfWeight = 0.7
	const parentWeight = 0.3

	var result [NumDims]float64
	for i := 0; i < NumDims; i++ {
		result[i] = selfWeight*vw.Weights[i] + parentWeight*parent[i]
	}
	return result
}
