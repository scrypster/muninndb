package main

import "testing"

// TestNoCommandSendsDefaultCredentialsToAnotherMachine: the default admin
// credentials are tried without being asked only on this machine, by EVERY
// command. The listing commands and the shell kept that rule; the sign-in every
// other admin subcommand uses, and the one inside `muninn init`, did not:
// `vault list` printed that the default credentials are not sent to another
// machine, and `vault create`, run next, posted them there.
func TestNoCommandSendsDefaultCredentialsToAnotherMachine(t *testing.T) {
	const there = "db.example.test"

	t.Run("the sign-in every admin subcommand uses", func(t *testing.T) {
		w := newWire(t, "")
		useBases(t, "http://"+there+":8475", "")
		// No password given and no terminal to ask on: the sign-in fails. What
		// matters is what was sent on the way.
		_ = authenticateAdmin("root", "", false)
		if logins := w.loginsAt(there); len(logins) != 0 {
			t.Errorf("default credentials were posted to another machine without being asked: logins as %q", logins)
		}
	})

	t.Run("credentials the user gave are sent where the user pointed", func(t *testing.T) {
		w := newWire(t, "")
		useBases(t, "http://"+there+":8475", "")
		_ = authenticateAdmin("harbourmaster", "given-on-the-command-line", false)
		if logins := w.loginsAt(there); len(logins) != 1 || logins[0] != "harbourmaster" {
			t.Errorf("logins at the other machine = %q; want the one the user asked for", logins)
		}
	})

	t.Run("on this machine the default credentials are still tried first", func(t *testing.T) {
		w := newWire(t, "127.0.0.1")
		useBases(t, "", "http://127.0.0.1:9476")
		if err := authenticateAdmin("root", "", false); err != nil {
			t.Errorf("authenticateAdmin on this machine with the default credentials: %v", err)
		}
		if logins := w.loginsAt("127.0.0.1"); len(logins) != 1 || logins[0] != "root" {
			t.Errorf("logins at this machine = %q; want one, as root", logins)
		}
	})

	t.Run("init's behaviour step", func(t *testing.T) {
		w := newWire(t, "")
		useBases(t, "http://"+there+":8475", "")
		captureStdout(func() { applyBehaviorToVault("balanced", "") })
		if logins := w.loginsAt(there); len(logins) != 0 {
			t.Errorf("init posted the default credentials to another machine: logins as %q", logins)
		}
	})
}
