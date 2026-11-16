package mbp_test

import (
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// subscribe sends a SUBSCRIBE and returns the subscription id the server
// answered with, or "" and the error it refused with.
func (c *mbpClient) subscribe(vault, chosenID string) (string, reply) {
	c.t.Helper()
	r := c.request(mbp.TypeSubscribe, &mbp.SubscribeRequest{
		Vault: vault, Context: []string{"berth closures"}, SubscriptionID: chosenID,
	})
	if r.isError() {
		return "", r
	}
	var resp mbp.SubscribeResponse
	if err := mbp.DecodeMsgpack(r.payload, &resp); err != nil {
		c.t.Fatalf("decode SUB_OK: %v", err)
	}
	return resp.SubID, r
}

func (c *mbpClient) unsubscribe(id string) reply {
	c.t.Helper()
	return c.request(mbp.TypeUnsub, &mbp.UnsubscribeRequest{SubID: id})
}

// awaitSubscriptions waits for a vault to have exactly want subscriptions.
func awaitSubscriptions(t *testing.T, env *matrixEnv, vault string, want int, when string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := len(env.subscriptionIDs(vault))
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: vault %s has %d subscriptions; want %d", when, vault, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSubscriptionsBelongToTheirConnection pins who may do what to a
// subscription over MBP, against a real engine, looking at what is actually
// registered rather than at what a client was told:
//
//   - UNSUB reaches only a subscription made on the same connection. It does
//     nothing to another connection's, in the same vault or any other.
//   - The server chooses subscription ids. A client cannot pick one, so it
//     cannot pick someone else's.
//   - A connection's subscriptions end when it does.
//   - One connection cannot take every subscription a vault allows.
//
// Before this, UNSUB removed whatever subscription had the id it named, on any
// vault; a client-chosen id replaced another vault's subscription of the same
// id in the registry; a subscription outlived the connection that made it; and
// one connection could register subscriptions until the vault's limit was
// reached and nobody else could subscribe to it.
func TestSubscriptionsBelongToTheirConnection(t *testing.T) {
	env := newMatrixEnv(t)
	const ours, theirs = "pilot-station", "customs-ledger"
	for _, v := range []string{ours, theirs} {
		if err := env.store.SetVaultConfig(auth.VaultConfig{Name: v, Public: false}); err != nil {
			t.Fatalf("configure %s: %v", v, err)
		}
	}
	key := func(vault, mode string) string {
		token, _, err := env.store.GenerateAPIKey(vault, "subs-"+mode, mode, nil)
		if err != nil {
			t.Fatalf("GenerateAPIKey: %v", err)
		}
		return token
	}

	t.Run("a connection ends its own subscription", func(t *testing.T) {
		c := dialMBP(t, env.addr, key(ours, auth.ModeObserve), ours)
		id, r := c.subscribe(ours, "")
		if id == "" {
			t.Fatalf("SUBSCRIBE refused: %d %q", r.code, r.message)
		}
		awaitSubscriptions(t, env, ours, 1, "after SUBSCRIBE")
		if r := c.unsubscribe(id); r.frameType != mbp.TypeUnsubOK {
			t.Fatalf("UNSUB of its own subscription answered frame 0x%02X %q", r.frameType, r.message)
		}
		awaitSubscriptions(t, env, ours, 0, "after UNSUB of its own subscription")
	})

	t.Run("UNSUB does nothing to another connection's subscription", func(t *testing.T) {
		owner := dialMBP(t, env.addr, key(ours, auth.ModeObserve), ours)
		id, _ := owner.subscribe(ours, "")
		awaitSubscriptions(t, env, ours, 1, "after the owner subscribed")

		// Same vault, even the same kind of key, but another connection.
		other := dialMBP(t, env.addr, key(ours, auth.ModeFull), ours)
		r := other.unsubscribe(id)
		if got := env.subscriptionIDs(ours); len(got) != 1 || got[0] != id {
			t.Errorf("another connection's UNSUB (answered frame 0x%02X) left the vault with subscriptions %v; want the owner's %q untouched", r.frameType, got, id)
		}
		// And it is told nothing about whether that id exists: the answer is
		// the one an id nobody holds gets.
		nobody := other.unsubscribe("no-such-subscription")
		if r.frameType != nobody.frameType || r.code != nobody.code || r.message != nobody.message {
			t.Errorf("UNSUB of someone else's id answered 0x%02X %d %q, of an id nobody holds 0x%02X %d %q; want no difference",
				r.frameType, r.code, r.message, nobody.frameType, nobody.code, nobody.message)
		}
		owner.unsubscribe(id)
		awaitSubscriptions(t, env, ours, 0, "after the owner unsubscribed")
	})

	t.Run("UNSUB does nothing to a subscription in another vault", func(t *testing.T) {
		// Held the way a REST or gRPC subscriber holds one.
		id, err := env.eng.SubscribeWithDeliver(fullCtx(), &mbp.SubscribeRequest{Vault: theirs, Context: []string{"manifests"}}, nil)
		if err != nil {
			t.Fatalf("subscribe in %s: %v", theirs, err)
		}
		defer env.eng.Unsubscribe(fullCtx(), id)

		c := dialMBP(t, env.addr, key(ours, auth.ModeObserve), ours)
		c.unsubscribe(id)
		if got := env.subscriptionIDs(theirs); len(got) != 1 {
			t.Errorf("a key pinned to %s sent UNSUB for a subscription in %s, which now has %v; want it untouched", ours, theirs, got)
		}
	})

	t.Run("a client cannot choose a subscription id", func(t *testing.T) {
		const chosen = "ledger-watch"
		victim, err := env.eng.SubscribeWithDeliver(fullCtx(), &mbp.SubscribeRequest{Vault: theirs, Context: []string{"manifests"}, SubscriptionID: chosen}, nil)
		if err != nil {
			t.Fatalf("subscribe in %s: %v", theirs, err)
		}
		defer env.eng.Unsubscribe(fullCtx(), victim)

		attacker := dialMBP(t, env.addr, key(ours, auth.ModeObserve), ours)
		mine, r := attacker.subscribe(ours, chosen)
		if mine == "" {
			t.Fatalf("SUBSCRIBE refused: %d %q", r.code, r.message)
		}
		if mine == chosen || victim == chosen {
			t.Errorf("a subscription was registered under the client's chosen id %q (victim %q, attacker %q)", chosen, victim, mine)
		}
		if mine == victim {
			t.Errorf("two subscriptions in two vaults share the id %q", mine)
		}
		if got := env.subscriptionIDs(theirs); len(got) != 1 || got[0] != victim {
			t.Errorf("%s has subscriptions %v; want exactly the victim's %q", theirs, got, victim)
		}
		if got := env.subscriptionIDs(ours); len(got) != 1 || got[0] != mine {
			t.Errorf("%s has subscriptions %v; want exactly the attacker's own %q", ours, got, mine)
		}

		// The victim ending its own subscription ends its own, not the attacker's.
		if err := env.eng.Unsubscribe(fullCtx(), victim); err != nil {
			t.Fatal(err)
		}
		if got := env.subscriptionIDs(theirs); len(got) != 0 {
			t.Errorf("after the victim unsubscribed, %s still has %v", theirs, got)
		}
		if got := env.subscriptionIDs(ours); len(got) != 1 {
			t.Errorf("the victim unsubscribing changed %s to %v", ours, got)
		}
		attacker.conn.Close()
		awaitSubscriptions(t, env, ours, 0, "after the attacker's connection closed")
	})

	t.Run("subscriptions end with their connection", func(t *testing.T) {
		c := dialMBP(t, env.addr, key(ours, auth.ModeObserve), ours)
		for i := 0; i < 3; i++ {
			if id, r := c.subscribe(ours, ""); id == "" {
				t.Fatalf("SUBSCRIBE %d refused: %d %q", i+1, r.code, r.message)
			}
		}
		awaitSubscriptions(t, env, ours, 3, "with the connection open")
		c.conn.Close()
		awaitSubscriptions(t, env, ours, 0, "after the connection closed")
	})

	t.Run("one connection cannot take a vault's whole subscription limit", func(t *testing.T) {
		c := dialMBP(t, env.addr, key(ours, auth.ModeObserve), ours)
		granted := 0
		var last reply
		for i := 0; i < 150; i++ { // the vault's own limit is 100
			id, r := c.subscribe(ours, "")
			if id == "" {
				last = r
				continue
			}
			granted++
		}
		t.Logf("150 SUBSCRIBEs on one connection: %d granted; then refused with %d %q", granted, last.code, last.message)
		if granted > 10 {
			t.Errorf("one connection was granted %d subscriptions in a vault that allows 100 in all", granted)
		}
		// Somebody else can still subscribe to the vault.
		id, err := env.eng.SubscribeWithDeliver(fullCtx(), &mbp.SubscribeRequest{Vault: ours, Context: []string{"tug roster"}}, nil)
		if err != nil {
			t.Errorf("another subscriber was refused (%v) after one connection had its fill", err)
		} else {
			env.eng.Unsubscribe(fullCtx(), id)
		}
		// Ending one of its own gives the connection room for another.
		c.conn.Close()
		awaitSubscriptions(t, env, ours, 0, "after the connection closed")
	})

	t.Run("every kind of session that may subscribe", func(t *testing.T) {
		const open = "harbour-notices"
		if err := env.store.SetVaultConfig(auth.VaultConfig{Name: open, Public: true}); err != nil {
			t.Fatal(err)
		}
		for _, p := range []struct{ name, token, vault string }{
			{"full", key(ours, auth.ModeFull), ours},
			{"append", key(ours, auth.ModeAppend), ours},
			{"observe", key(ours, auth.ModeObserve), ours},
			{"no key, public vault", "", open},
		} {
			c := dialMBP(t, env.addr, p.token, p.vault)
			id, r := c.subscribe(p.vault, "")
			if id == "" {
				t.Errorf("%s: SUBSCRIBE refused: %d %q", p.name, r.code, r.message)
				continue
			}
			awaitSubscriptions(t, env, p.vault, 1, p.name+": after SUBSCRIBE")
			c.unsubscribe(id)
			awaitSubscriptions(t, env, p.vault, 0, p.name+": after UNSUB")
		}
		// An ingest-only key may do neither (SEC-20).
		w := dialMBP(t, env.addr, key(ours, auth.ModeWrite), ours)
		if id, r := w.subscribe(ours, ""); id != "" || r.code != mbp.ErrAuthFailed {
			t.Errorf("write key: SUBSCRIBE answered %q / %d %q; want it refused", id, r.code, r.message)
		}
	})
}
