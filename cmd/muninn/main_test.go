//go:build !integration

package main

import (
	"os"
	"testing"
)

// TestMain installs the hermetic guard (hermetic_test.go) before any test in
// the package runs, and fails the run if any test tried to dial a default
// daemon port or a non-loopback host. The integration build has its own
// TestMain (integration_test.go), which installs the same guard.
func TestMain(m *testing.M) {
	os.Exit(runHermetic(m))
}
