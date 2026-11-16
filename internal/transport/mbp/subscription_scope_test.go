package mbp_test

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/trigger"
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

// awaitSubscriptions waits for a vault to have exactly want subscriptions
// registered with the engine.
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

// delivered is a delivery function that takes a push and does nothing with it.
// A subscription made with one is a real subscription, held the way a REST or
// gRPC subscriber holds one: it is registered and counts against its vault.
func delivered(context.Context, *trigger.ActivationPush) error { return nil }

// TestSubscriptionsOverMBP pins what a SUBSCRIBE over MBP is and is not,
// against a real engine, looking at what is actually registered rather than at
// what a client was told (SEC-26).
//
// Nothing has ever been delivered over MBP: the server writes no ACTIVATION
// frame. So a SUBSCRIBE there is answered and remembered by its connection, and
// registers nothing with the engine:
//
//   - It takes none of the subscription places its vault allows, however many
//     connections send it. Those places are for subscribers who can be
//     delivered to.
//   - A connection may hold a bounded number.
//   - UNSUB reaches only what the same connection subscribed. It does nothing
//     to another connection's subscription or to a real one, in any vault.
//   - The server chooses subscription ids.
//
// Before this an MBP subscription was registered like a real one. Thirteen
// connections with no key, eight subscriptions each, filled a public vault's
// limit of 100, and every REST and gRPC subscriber after that was refused.
// Earlier still, UNSUB removed whatever subscription had the id it named, on
// any vault, and a subscription outlived the connection that made it.
func TestSubscriptionsOverMBP(t *testing.T) {
	env := newMatrixEnv(t)
	const ours, theirs, open = "pilot-station", "customs-ledger", "harbour-notices"
	for vault, public := range map[string]bool{ours: false, theirs: false, open: true} {
		if err := env.store.SetVaultConfig(auth.VaultConfig{Name: vault, Public: public}); err != nil {
			t.Fatalf("configure %s: %v", vault, err)
		}
	}
	key := func(vault, mode string) string {
		token, _, err := env.store.GenerateAPIKey(vault, "subs-"+mode, mode, nil)
		if err != nil {
			t.Fatalf("GenerateAPIKey: %v", err)
		}
		return token
	}
	// realSubscriber subscribes the way REST and gRPC do, and reports whether
	// the vault had room.
	realSubscriber := func(t *testing.T, vault string) (string, error) {
		t.Helper()
		id, err := env.eng.SubscribeWithDeliver(fullCtx(), &mbp.SubscribeRequest{Vault: vault, Context: []string{"tug roster"}}, delivered)
		if err == nil {
			t.Cleanup(func() { _ = env.eng.Unsubscribe(fullCtx(), id) })
		}
		return id, err
	}

	t.Run("it takes none of the vault's subscription places, from any number of connections", func(t *testing.T) {
		granted := 0
		for c := 0; c < 13; c++ { // 13 x 8 = 104, past the vault's limit of 100
			client := dialMBP(t, env.addr, "", open)
			for i := 0; i < 8; i++ {
				if id, _ := client.subscribe(open, ""); id != "" {
					granted++
				}
			}
		}
		if granted != 104 {
			t.Errorf("13 connections x 8 SUBSCRIBEs: %d answered SUB_OK; want all 104", granted)
		}
		if got := env.subscriptionIDs(open); len(got) != 0 {
			t.Errorf("the vault has %d subscriptions registered after %d SUBSCRIBEs over MBP; want none", len(got), granted)
		}
		if _, err := realSubscriber(t, open); err != nil {
			t.Errorf("a subscriber who can be delivered to was refused after MBP connections subscribed: %v", err)
		}
	})

	t.Run("a connection holds a bounded number, and ending one makes room", func(t *testing.T) {
		c := dialMBP(t, env.addr, key(ours, auth.ModeObserve), ours)
		var ids []string
		var last reply
		for i := 0; i < 150; i++ {
			id, r := c.subscribe(ours, "")
			if id == "" {
				last = r
				continue
			}
			ids = append(ids, id)
		}
		if len(ids) != 8 {
			t.Fatalf("150 SUBSCRIBEs on one connection: %d granted; want 8", len(ids))
		}
		if last.code != mbp.ErrRateLimited || last.message != "subscription limit reached for this connection" {
			t.Errorf("past the limit: %d %q", last.code, last.message)
		}
		if r := c.unsubscribe(ids[0]); r.frameType != mbp.TypeUnsubOK {
			t.Fatalf("UNSUB of its own subscription answered frame 0x%02X %q", r.frameType, r.message)
		}
		if id, r := c.subscribe(ours, ""); id == "" {
			t.Errorf("after ending one of its own, the connection was still refused: %d %q", r.code, r.message)
		}
		if id, r := c.subscribe(ours, ""); id != "" {
			t.Errorf("and it was then granted a ninth (%q, frame 0x%02X)", id, r.frameType)
		}
	})

	t.Run("UNSUB does nothing to another connection's subscription", func(t *testing.T) {
		owner := dialMBP(t, env.addr, key(ours, auth.ModeObserve), ours)
		var id string
		for i := 0; i < 8; i++ { // the owner is at its limit
			id, _ = owner.subscribe(ours, "")
		}
		// Same vault, even the same kind of key, but another connection.
		other := dialMBP(t, env.addr, key(ours, auth.ModeFull), ours)
		r := other.unsubscribe(id)
		if got, refusal := owner.subscribe(ours, ""); got != "" {
			t.Errorf("another connection's UNSUB (answered frame 0x%02X) ended the owner's subscription: the owner, at its limit before, was granted another (%q)", r.frameType, got)
		} else if refusal.code != mbp.ErrRateLimited {
			t.Errorf("the owner's ninth SUBSCRIBE was refused with %d %q", refusal.code, refusal.message)
		}
		// And it is told nothing about whether that id exists: the answer is
		// the one an id nobody holds gets.
		nobody := other.unsubscribe("no-such-subscription")
		if r.frameType != nobody.frameType || r.code != nobody.code || r.message != nobody.message {
			t.Errorf("UNSUB of someone else's id answered 0x%02X %d %q, of an id nobody holds 0x%02X %d %q; want no difference",
				r.frameType, r.code, r.message, nobody.frameType, nobody.code, nobody.message)
		}
	})

	t.Run("UNSUB does nothing to a real subscription, in the same vault or another", func(t *testing.T) {
		for _, vault := range []string{ours, theirs} {
			id, err := realSubscriber(t, vault)
			if err != nil {
				t.Fatalf("subscribe in %s: %v", vault, err)
			}
			c := dialMBP(t, env.addr, key(ours, auth.ModeFull), ours)
			c.unsubscribe(id)
			if got := env.subscriptionIDs(vault); len(got) != 1 || got[0] != id {
				t.Errorf("a key pinned to %s sent UNSUB for a real subscription in %s, which now has %v; want it untouched", ours, vault, got)
			}
			_ = env.eng.Unsubscribe(fullCtx(), id)
			awaitSubscriptions(t, env, vault, 0, "after the real subscriber left "+vault)
		}
	})

	t.Run("a client cannot choose a subscription id", func(t *testing.T) {
		const chosen = "ledger-watch"
		victim, err := env.eng.SubscribeWithDeliver(fullCtx(), &mbp.SubscribeRequest{Vault: theirs, Context: []string{"manifests"}, SubscriptionID: chosen}, delivered)
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
			t.Errorf("a subscription was given the client's chosen id %q (victim %q, attacker %q)", chosen, victim, mine)
		}
		if mine == victim {
			t.Errorf("two subscriptions share the id %q", mine)
		}
		attacker.unsubscribe(chosen)
		attacker.unsubscribe(victim)
		if got := env.subscriptionIDs(theirs); len(got) != 1 || got[0] != victim {
			t.Errorf("%s has subscriptions %v; want exactly the victim's %q", theirs, got, victim)
		}
	})

	t.Run("nothing is left registered when a connection closes", func(t *testing.T) {
		c := dialMBP(t, env.addr, key(ours, auth.ModeObserve), ours)
		for i := 0; i < 3; i++ {
			if id, r := c.subscribe(ours, ""); id == "" {
				t.Fatalf("SUBSCRIBE %d refused: %d %q", i+1, r.code, r.message)
			}
		}
		c.conn.Close()
		awaitSubscriptions(t, env, ours, 0, "after the connection closed")
	})

	t.Run("every kind of session that may subscribe", func(t *testing.T) {
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
			if r := c.unsubscribe(id); r.frameType != mbp.TypeUnsubOK {
				t.Errorf("%s: UNSUB answered frame 0x%02X %q", p.name, r.frameType, r.message)
			}
		}
		// An ingest-only key may do neither (SEC-20).
		w := dialMBP(t, env.addr, key(ours, auth.ModeWrite), ours)
		if id, r := w.subscribe(ours, ""); id != "" || r.code != mbp.ErrAuthFailed {
			t.Errorf("write key: SUBSCRIBE answered %q / %d %q; want it refused", id, r.code, r.message)
		}
	})
}
