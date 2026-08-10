package storage

import (
	"fmt"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// Options for opening a Pebble database.
type Options struct {
	MaxOpenFiles          int
	MemTableSize          uint64
	L0CompactionThreshold int
	L0StopWritesThreshold int
	LBaseMaxBytes         int64
	DisableWAL            bool
}

// DefaultOptions returns sensible defaults for a MuninnDB Pebble instance.
func DefaultOptions() *Options {
	return &Options{
		MaxOpenFiles:          1000,
		MemTableSize:          64 * 1024 * 1024, // 64MB
		L0CompactionThreshold: 4,
		L0StopWritesThreshold: 12,
		LBaseMaxBytes:         256 * 1024 * 1024, // 256MB
		DisableWAL:            false,
	}
}

// OpenPebble opens a Pebble database at the specified path.
func OpenPebble(path string, opts *Options) (*pebble.DB, error) {
	if opts == nil {
		opts = DefaultOptions()
	}

	pebbleOpts := &pebble.Options{
		MaxOpenFiles:  opts.MaxOpenFiles,
		MemTableSize:  opts.MemTableSize,
		LBaseMaxBytes: opts.LBaseMaxBytes,
		DisableWAL:    opts.DisableWAL,
	}

	pebbleOpts.L0CompactionThreshold = opts.L0CompactionThreshold
	pebbleOpts.L0StopWritesThreshold = opts.L0StopWritesThreshold

	return pebble.Open(path, pebbleOpts)
}

// BatchSet adds a Set operation to a batch.
func BatchSet(batch *pebble.Batch, key, value []byte) {
	batch.Set(key, value, nil)
}

// BatchDelete adds a Delete operation to a batch.
func BatchDelete(batch *pebble.Batch, key []byte) {
	batch.Delete(key, nil)
}

// PrefixIterator creates an iterator bounded by a prefix.
//
// STO-11: the bound comes from keys.PrefixUpperBound. This used to open-code a
// byte-identical COPY of that helper's pre-#816 carry loop — increment the
// first sub-0xFF byte from the right, keep the trailing 0xFFs — which is why
// the STO-12 census had to name PrefixIterator alongside the helper as carrying
// the same over-inclusive bound. One rule, one implementation.
func PrefixIterator(db *pebble.DB, prefix []byte) (*pebble.Iterator, error) {
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: keys.PrefixUpperBound(prefix),
	})
	if err != nil {
		return nil, fmt.Errorf("prefix iterator: %w", err)
	}
	return iter, nil
}

// Get retrieves a single key from the database.
func Get(db *pebble.DB, key []byte) ([]byte, error) {
	return getFromReader(db, key)
}

// getFromReader retrieves a single key from any pebble.Reader (DB or Snapshot).
func getFromReader(r pebble.Reader, key []byte) ([]byte, error) {
	val, closer, err := r.Get(key)
	if err != nil {
		if err == pebble.ErrNotFound {
			return nil, nil
		}
		return nil, err
	}
	defer closer.Close()

	result := make([]byte, len(val))
	copy(result, val)
	return result, nil
}

// MultiGet retrieves multiple keys from the database.
func MultiGet(db *pebble.DB, keys [][]byte) ([][]byte, error) {
	results := make([][]byte, len(keys))

	for i, key := range keys {
		val, closer, err := db.Get(key)
		if err != nil {
			if err == pebble.ErrNotFound {
				results[i] = nil
				continue
			}
			return nil, err
		}

		// Make a copy since the underlying slice may be reused
		result := make([]byte, len(val))
		copy(result, val)
		results[i] = result
		closer.Close()
	}

	return results, nil
}
