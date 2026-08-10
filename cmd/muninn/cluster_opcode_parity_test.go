package main

import (
	"testing"

	"github.com/scrypster/muninndb/internal/replication"
)

// TestAuthReplicationOpCodesMatchWALOps pins the op codes internal/auth pins by
// hand (it cannot import internal/replication without a cycle) against the real
// WALOp values. If a WALOp value ever changes, configuration writes would ship
// under the wrong op code and be applied — or filtered — incorrectly.
//
// The values are duplicated in internal/auth/cluster.go as opSet/opDelete/opBatch.
func TestAuthReplicationOpCodesMatchWALOps(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth uint8
		wal  replication.WALOp
	}{
		{"opSet", 1, replication.OpSet},
		{"opDelete", 2, replication.OpDelete},
		{"opBatch", 3, replication.OpBatch},
	} {
		if uint8(tc.wal) != tc.auth {
			t.Errorf("%s: internal/auth pins %d, replication.WALOp is %d — "+
				"update internal/auth/cluster.go", tc.name, tc.auth, uint8(tc.wal))
		}
	}
}
