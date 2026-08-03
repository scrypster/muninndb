//go:build !race

package engine

// raceBuild is false for a normal (non -race) build/test run. See
// race_detect_on_test.go.
const raceBuild = false
