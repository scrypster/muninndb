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

// TestSubscribeOverMBPIsRefused pins what a SUBSCRIBE over MBP is answered
// with (SEC-26). Nothing has ever been delivered over MBP: the server writes
// no ACTIVATION frame. A SUBSCRIBE there used to be answered SUB_OK with the
// status "active" all the same, and the client then waited for pushes that
// could not come. It is now refused, in words that say where subscriptions do
// work.
//
// It also keeps what the earlier fixes established, which refusing makes true
// by construction: nothing sent over MBP takes one of the subscription places
// a vault allows, and nothing sent over MBP ends a subscription held
// elsewhere, in any vault.
func TestSubscribeOverMBPIsRefused(t *testing.T) {
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
	realSubscriber := func(t *testing.T, vault string) string {
		t.Helper()
		id, err := env.eng.SubscribeWithDeliver(fullCtx(), &mbp.SubscribeRequest{Vault: vault, Context: []string{"tug roster"}}, delivered)
		if err != nil {
			t.Fatalf("a subscriber who can be delivered to was refused in %s: %v", vault, err)
		}
		t.Cleanup(func() { _ = env.eng.Unsubscribe(fullCtx(), id) })
		return id
	}

	t.Run("every session that may read is told where subscriptions work", func(t *testing.T) {
		for _, p := range []struct{ name, token, vault string }{
			{"full", key(ours, auth.ModeFull), ours},
			{"append", key(ours, auth.ModeAppend), ours},
			{"observe", key(ours, auth.ModeObserve), ours},
			{"no key, public vault", "", open},
		} {
			c := dialMBP(t, env.addr, p.token, p.vault)
			id, r := c.subscribe(p.vault, "")
			if id != "" {
				t.Errorf("%s: SUBSCRIBE was answered with a subscription id (%q)", p.name, id)
				continue
			}
			if r.code != mbp.ErrSubscriptionNotFound || r.message != subscribeNotServed {
				t.Errorf("%s: SUBSCRIBE refused with %d %q; want %d %q", p.name, r.code, r.message, mbp.ErrSubscriptionNotFound, subscribeNotServed)
			}
			// The connection carries on.
			if r := c.request(mbp.TypePing, &mbp.PingRequest{Data: "watch"}); r.frameType != mbp.TypePong {
				t.Errorf("%s: after the refusal, PING answered frame 0x%02X %q", p.name, r.frameType, r.message)
			}
		}
	})

	t.Run("an ingest-only key is refused as that, first", func(t *testing.T) {
		w := dialMBP(t, env.addr, key(ours, auth.ModeWrite), ours)
		if id, r := w.subscribe(ours, ""); id != "" || r.code != mbp.ErrAuthFailed {
			t.Errorf("write key: SUBSCRIBE answered %q / %d %q; want the mode refusal (SEC-20)", id, r.code, r.message)
		}
	})

	t.Run("a session naming a vault it may not use is refused as that, first", func(t *testing.T) {
		c := dialMBP(t, env.addr, key(ours, auth.ModeFull), ours)
		if id, r := c.subscribe(theirs, ""); id != "" || r.code != mbp.ErrAuthFailed {
			t.Errorf("SUBSCRIBE naming another vault answered %q / %d %q; want the vault refusal", id, r.code, r.message)
		}
	})

	t.Run("it takes none of a vault's subscription places, from any number of connections", func(t *testing.T) {
		for c := 0; c < 13; c++ {
			client := dialMBP(t, env.addr, "", open)
			for i := 0; i < 8; i++ {
				client.subscribe(open, "")
			}
		}
		if got := env.subscriptionIDs(open); len(got) != 0 {
			t.Errorf("the vault has %d subscriptions registered after 104 SUBSCRIBEs over MBP; want none", len(got))
		}
		realSubscriber(t, open)
	})

	t.Run("UNSUB ends nothing, in the same vault or another, and says nothing about what exists", func(t *testing.T) {
		c := dialMBP(t, env.addr, key(ours, auth.ModeFull), ours)
		nobody := c.unsubscribe("no-such-subscription")
		for _, vault := range []string{ours, theirs} {
			id := realSubscriber(t, vault)
			r := c.unsubscribe(id)
			if got := env.subscriptionIDs(vault); len(got) != 1 || got[0] != id {
				t.Errorf("UNSUB over MBP for a real subscription in %s left it with %v; want it untouched", vault, got)
			}
			if r.frameType != nobody.frameType || r.code != nobody.code || r.message != nobody.message {
				t.Errorf("UNSUB of a real id answered 0x%02X %d %q, of an id nobody holds 0x%02X %d %q; want no difference",
					r.frameType, r.code, r.message, nobody.frameType, nobody.code, nobody.message)
			}
			_ = env.eng.Unsubscribe(fullCtx(), id)
			awaitSubscriptions(t, env, vault, 0, "after the real subscriber left "+vault)
		}
	})
}
