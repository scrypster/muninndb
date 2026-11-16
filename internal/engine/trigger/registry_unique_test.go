package trigger

import "testing"

// TestRegistry_AnIDIsRegisteredOnce: a second subscription offered under an id
// that is already registered is refused and changes nothing. Replacing the
// entry would leave the first subscription listed for its vault with nothing
// able to remove it, and the first owner's unsubscribe would then remove the
// second one instead.
func TestRegistry_AnIDIsRegisteredOnce(t *testing.T) {
	r := newRegistry()
	first := &Subscription{ID: "watch", VaultID: 1}
	second := &Subscription{ID: "watch", VaultID: 2} // another vault, the same id

	if !r.Add(first) {
		t.Fatal("the first subscription was refused")
	}
	if r.Add(second) {
		t.Fatal("a second subscription was registered under an id already in use")
	}
	if got, _ := r.Get("watch"); got != first {
		t.Errorf("the id now names %+v; want the first subscription", got)
	}
	if n := r.CountForVault(1); n != 1 {
		t.Errorf("vault 1 has %d subscriptions; want 1", n)
	}
	if n := r.CountForVault(2); n != 0 {
		t.Errorf("vault 2 has %d subscriptions; want 0", n)
	}
	if n := r.CountTotal(); n != 1 {
		t.Errorf("%d subscriptions in all; want 1", n)
	}

	// Removing it removes the one that was registered, and the id is free again.
	r.Remove("watch")
	if n := r.CountTotal(); n != 0 || len(r.ForVault(1)) != 0 {
		t.Errorf("after Remove: %d in all, %d listed for vault 1", n, len(r.ForVault(1)))
	}
	if !r.Add(second) {
		t.Errorf("the id could not be registered again after it was removed")
	}
}

// TestTriggerSystem_SubscribeRefusesATakenID is the same through Subscribe,
// which is what the engine calls.
func TestTriggerSystem_SubscribeRefusesATakenID(t *testing.T) {
	ts := &TriggerSystem{registry: newRegistry()}
	if err := ts.Subscribe(&Subscription{ID: "watch", VaultID: 1}); err != nil {
		t.Fatalf("first Subscribe: %v", err)
	}
	if err := ts.Subscribe(&Subscription{ID: "watch", VaultID: 2}); err != ErrSubscriptionIDTaken {
		t.Fatalf("second Subscribe under the same id: err = %v; want ErrSubscriptionIDTaken", err)
	}
}
