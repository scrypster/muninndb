package replication

import (
	"encoding/binary"
	"hash/crc32"
)

var entrySumTable = crc32.MakeTable(crc32.Castagnoli)

// entrySum is the checksum of one replication entry, over everything that
// identifies it: its sequence number, operation, timestamp, key and value.
//
// A follower keeps the checksum of the last entry it applied next to its apply
// cursor, and a leader compares that with the entry it holds at the same
// sequence number (JoinHandler.divergence). Two leaders can number different
// entries alike, and the sequence number alone cannot tell their histories
// apart. The timestamp is included so that two leaders writing the same bytes
// at the same number still differ.
//
// Both sides compute it from the same fields: the leader from its stored log
// entry, the follower from the entry as streamed. It is a CRC, not a digest.
// A collision can only make two different entries look alike (one in four
// billion), which misses a refusal. It can never make equal entries look
// different, so it cannot cause a refusal.
func entrySum(e ReplicationEntry) uint32 {
	var head [21]byte
	binary.BigEndian.PutUint64(head[0:8], e.Seq)
	head[8] = byte(e.Op)
	binary.BigEndian.PutUint64(head[9:17], uint64(e.TimestampNS))
	binary.BigEndian.PutUint32(head[17:21], uint32(len(e.Key)))
	sum := crc32.Update(0, entrySumTable, head[:])
	sum = crc32.Update(sum, entrySumTable, e.Key)
	return crc32.Update(sum, entrySumTable, e.Value)
}
