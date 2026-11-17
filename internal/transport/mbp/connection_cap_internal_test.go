package mbp

import (
	"math"
	"testing"
)

// TestConnMemoryBoundIsTheSumOfWhatBoundsIt pins the figure the connection cap
// is derived from to the limits that make it true, so that changing one of
// them changes the cap with it instead of leaving a number behind.
func TestConnMemoryBoundIsTheSumOfWhatBoundsIt(t *testing.T) {
	const mib = 1 << 20
	requests := connRequestGate + MaxPayloadSize // the gate, and the one frame read past it
	decoded := connDecodeBudget
	replies := connReplyBudget + replyQueueLen*smallReply
	if requests != 20*mib || decoded != 32*mib || replies != 17*mib {
		t.Errorf("requests %d MiB, decoded %d MiB, replies %d MiB; the derivation's comment says 20, 32 and 17", requests/mib, decoded/mib, replies/mib)
	}
	if connMemoryBound != requests+decoded+replies {
		t.Errorf("connMemoryBound = %d; want the sum %d", connMemoryBound, requests+decoded+replies)
	}
	// Every frame being handled can be answered without the queue overflowing.
	if replyQueueLen <= maxConcurrentFrames {
		t.Errorf("the reply queue (%d) cannot hold an answer for every frame in flight (%d)", replyQueueLen, maxConcurrentFrames)
	}
	// The largest reply reserved for fits the budget it is reserved from.
	if got := activateReplyBound(1 << 30); got != connReplyBudget {
		t.Errorf("activateReplyBound(huge) = %d; want the whole budget, %d", got, connReplyBudget)
	}
	if got := activateReplyBound(0); got != smallReply+defaultActivateResults*activateItemBound {
		t.Errorf("activateReplyBound(0) = %d; want the default result count's worth", got)
	}
}

// TestDerivedMaxConnections pins where the default connection cap comes from:
// the number of connections, each at connMemoryBound, that fit in a quarter of
// the process's memory limit.
func TestDerivedMaxConnections(t *testing.T) {
	const gib = int64(1) << 30
	cases := []struct {
		name  string
		limit int64
		want  int
	}{
		{"the default 4 GiB limit: 14 fit, the floor is 16", 4 * gib, 16},
		{"16 GiB", 16 * gib, 59},
		{"64 GiB", 64 * gib, 237},
		{"1 GiB", 1 * gib, 16},
		{"a limit too small to derive from keeps the server usable", 256 << 20, minDerivedConnections},
		{"no memory limit set", math.MaxInt64, fallbackMaxConnections},
		{"a nonsense limit", 0, fallbackMaxConnections},
	}
	for _, tc := range cases {
		if got := derivedMaxConnections(tc.limit); got != tc.want {
			t.Errorf("%s: derivedMaxConnections(%d) = %d, want %d", tc.name, tc.limit, got, tc.want)
		}
	}
	// The fallback is the derivation at the default limit, not a second number.
	if got := derivedMaxConnections(4 * gib); got != fallbackMaxConnections {
		t.Errorf("fallbackMaxConnections = %d, but the default limit derives %d", fallbackMaxConnections, got)
	}
}

// TestResolveMaxConnections_Override: the override wins when it is a positive
// number, and anything else is refused in favour of the default rather than
// read as "no limit".
func TestResolveMaxConnections_Override(t *testing.T) {
	t.Setenv(maxConnectionsEnv, "")
	def := resolveMaxConnections()
	if def < minDerivedConnections {
		t.Fatalf("default = %d", def)
	}
	for value, want := range map[string]int{
		"300":   300,
		"1":     1,
		"0":     def,
		"-5":    def,
		"lots":  def,
		"12.5":  def,
		" 40 ":  def,
		"1e3":   def,
		"99999": 99999,
	} {
		t.Setenv(maxConnectionsEnv, value)
		if got := resolveMaxConnections(); got != want {
			t.Errorf("%s=%q: cap = %d, want %d", maxConnectionsEnv, value, got, want)
		}
	}
}
