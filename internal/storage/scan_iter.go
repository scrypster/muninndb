package storage

// scanIterator is the subset of *pebble.Iterator that the association range
// scans use. It exists so those scans can be handed a test-supplied iterator
// that FAILS PARTWAY THROUGH — the one condition a real Pebble DB will not
// produce on demand, and the exact condition #808 is about.
//
// *pebble.Iterator satisfies this interface as written; nothing in production
// implements it.
type scanIterator interface {
	First() bool
	SeekGE(key []byte) bool
	Next() bool
	Valid() bool
	Key() []byte
	Value() []byte
	Error() error
	Close() error
}

// scanIter returns the iterator a range scan should actually use, consulting
// the TEST-ONLY iterFault seam. In production iterFault is nil and this is an
// identity function; the AST census (TestIterFaultIsArmedOnlyByTests) pins that
// no non-test file ever assigns the field.
//
// scanPrefix is passed through so a fault can be scoped to one namespace
// instead of the whole store, the same way failReadsWithPrefix scopes readFault.
func (ps *PebbleStore) scanIter(scanPrefix []byte, it scanIterator) scanIterator {
	if ps.iterFault != nil {
		if replacement := ps.iterFault(scanPrefix, it); replacement != nil {
			return replacement
		}
	}
	return it
}
