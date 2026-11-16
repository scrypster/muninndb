//go:build !cognitiontrial

package hnsw

// legacySelectArm is the cognition-trial control switch for neighbour
// selection. In every shipped build it is the constant false: there is no
// runtime toggle, by design (decision record, plasticity kill-switches — a
// correctness fix that can be switched off invites switching it off when it
// misfires). The trial build's counterpart restores the pre-fix selection so
// one commit built twice yields both arms of an A/B.
func legacySelectArm() bool { return false }
