package muninn

import (
	"errors"
	"time"
)

// ErrNotFound is returned when an engram with the requested ID does not exist.
var ErrNotFound = errors.New("engram not found")

// Engram is a single memory record returned by the public API.
type Engram struct {
	ID         string
	Concept    string
	Content    string
	Summary    string
	State      string
	Score      float64
	Confidence float32
	Tags       []string
	CreatedAt  time.Time
	LastAccess time.Time
}
