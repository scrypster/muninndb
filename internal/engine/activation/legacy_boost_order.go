//go:build !cognitiontrial

package activation

// legacyBoostOrder is the content-first-order A/B control switch (COG-35). In
// every shipped build it is the constant false: there is no runtime toggle,
// by design (a correctness fix that can be switched off invites switching it
// off when it misfires — decision record, plasticity kill-switches).
func legacyBoostOrder() bool { return false }
