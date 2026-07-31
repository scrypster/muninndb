//go:build race

package engine

// raceBuild is true when the binary is built with -race. The race detector's
// instrumentation overhead (memory-access interception on every read/write)
// is large and unrelated to the real-world cost this increment's latency
// budget is measuring, so latency-budget tests widen their tolerance under a
// race build rather than flake on CI's -race job.
const raceBuild = true
