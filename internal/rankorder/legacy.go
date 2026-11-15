//go:build !cognitiontrial

package rankorder

// legacyAscending is the cognition-trial control switch. In every shipped
// build it is the constant false: there is no runtime toggle, by design (a
// correctness fix that can be switched off invites switching it off when it
// misfires — decision record, plasticity kill-switches).
func legacyAscending() bool { return false }
