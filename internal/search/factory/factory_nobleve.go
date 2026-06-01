//go:build !bleve

package factory

import (
	"errors"

	"github.com/scrypster/muninndb/internal/search"
)

// ErrBleveBackendUnavailable is returned when the bleve search backend is not
// compiled in (missing -tags bleve).
var ErrBleveBackendUnavailable = errors.New("bleve search backend requires -tags bleve")

// OpenBleve is a no-op stub that returns ErrBleveBackendUnavailable unless
// built with -tags bleve.
func OpenBleve(cfg BleveConfig) (search.Backend, error) {
	return nil, ErrBleveBackendUnavailable
}
