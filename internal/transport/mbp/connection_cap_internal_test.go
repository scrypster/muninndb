package mbp

import (
	"math"
	"testing"
)

// TestDerivedMaxConnections pins where the default connection cap comes from:
// the number of connections, each holding one frame's worth, that fit in a
// quarter of the process's memory limit.
func TestDerivedMaxConnections(t *testing.T) {
	const gib = int64(1) << 30
	cases := []struct {
		name  string
		limit int64
		want  int
	}{
		{"the default 4 GiB limit", 4 * gib, 64},
		{"16 GiB", 16 * gib, 256},
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
